// Package jobs owns durable execution primitives, not business state machines.
// Executors retain their existing publication/approval contracts.
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	ErrLeaseLost = errors.New("workflow lease lost or cancelled")
	ErrCapacity  = errors.New("workflow queue capacity reached")
	ErrConflict  = errors.New("workflow idempotency key has different frozen input")
)

type Input struct {
	Key  string `bson:"key" json:"key"`
	Data bson.M `bson:"data" json:"data"`
}
type Submission struct {
	Kind, Key, Actor, InputRef string
	Items                      []Input
	Deadline                   time.Time
}
type Job struct {
	Attempts        int                `bson:"attempts" json:"attempts"`
	ID              primitive.ObjectID `bson:"_id" json:"id"`
	Schema          int                `bson:"schema_version" json:"schema_version"`
	Kind            string             `bson:"kind" json:"kind"`
	Actor           string             `bson:"actor" json:"actor"`
	InputRef        string             `bson:"input_ref,omitempty" json:"input_ref,omitempty"`
	Digest          string             `bson:"digest" json:"-"`
	State           string             `bson:"state" json:"state"`
	Fence           int64              `bson:"fence" json:"fence"`
	Owner           string             `bson:"owner,omitempty" json:"-"`
	LeaseUntil      time.Time          `bson:"lease_until" json:"lease_until"`
	CreatedAt       time.Time          `bson:"created_at" json:"created_at"`
	Deadline        time.Time          `bson:"deadline_at" json:"deadline_at"`
	CancelRequested bool               `bson:"cancel_requested" json:"cancel_requested"`
	Total           int                `bson:"total" json:"total"`
	Completed       int                `bson:"completed" json:"completed"`
	ErrorCode       string             `bson:"error_code,omitempty" json:"error_code,omitempty"`
}
type Item struct {
	ID     string             `bson:"_id" json:"id"`
	JobID  primitive.ObjectID `bson:"job_id" json:"job_id"`
	Key    string             `bson:"key" json:"key"`
	Input  bson.M             `bson:"input" json:"-"`
	Done   bool               `bson:"done" json:"done"`
	Result bson.M             `bson:"result,omitempty" json:"result,omitempty"`
}
type Event struct {
	JobID primitive.ObjectID `bson:"job_id" json:"job_id"`
	At    time.Time          `bson:"at" json:"at"`
	State string             `bson:"state" json:"state"`
	Code  string             `bson:"code,omitempty" json:"code,omitempty"`
}
type Repository struct {
	DB       *database.DB
	Capacity int
}

// AppendItems must join an existing checkpoint transaction. Preparation is
// therefore replay-safe: either every frozen row and its marker exist, or none.
func (r *Repository) AppendItems(ctx mongo.SessionContext, j Job, inputs []Input) error {
	if ctx == nil {
		return fmt.Errorf("append requires checkpoint transaction")
	}
	seen := map[string]bool{}
	for _, input := range inputs {
		if input.Key == "" || seen[input.Key] {
			return fmt.Errorf("item keys must be non-empty and unique")
		}
		seen[input.Key] = true
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	if len(raw) > 8<<20 || len(inputs) > 10000 {
		return fmt.Errorf("workflow preparation exceeds input budget")
	}
	filter := leaseFilter(j)
	filter["total"] = bson.M{"$lte": 10000 - len(inputs)}
	result, err := r.DB.Collection("workflow_jobs").UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"total": len(inputs)}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrLeaseLost
	}
	for _, input := range inputs {
		if _, err := r.DB.Collection("workflow_items").InsertOne(ctx, Item{ID: j.ID.Hex() + ":" + input.Key, JobID: j.ID, Key: input.Key, Input: input.Data}); err != nil {
			return err
		}
	}
	return nil
}

func NewRepository(db *database.DB, capacity int) *Repository {
	if capacity < 1 {
		capacity = 256
	}
	return &Repository{DB: db, Capacity: capacity}
}

func (r *Repository) EnsureIndexes(ctx context.Context) error {
	for name, keys := range map[string][]mongo.IndexModel{
		"workflow_jobs":         {{Keys: bson.D{{Key: "kind", Value: 1}, {Key: "state", Value: 1}, {Key: "created_at", Value: 1}}}, {Keys: bson.D{{Key: "lease_until", Value: 1}}}, {Keys: bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}, {Keys: bson.D{{Key: "kind", Value: 1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}},
		"workflow_items":        {{Keys: bson.D{{Key: "job_id", Value: 1}, {Key: "key", Value: 1}}, Options: options.Index().SetUnique(true)}},
		"workflow_events":       {{Keys: bson.D{{Key: "job_id", Value: 1}, {Key: "at", Value: 1}}}},
		"import_jobs":           {{Keys: bson.D{{Key: "started_at", Value: -1}, {Key: "_id", Value: -1}}}},
		"template_upgrade_jobs": {{Keys: bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}},
		"approval_requests":     {{Keys: bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}},
	} {
		if _, err := r.DB.Collection(name).Indexes().CreateMany(ctx, keys); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) Submit(ctx context.Context, s Submission) (*Job, error) {
	if s.Kind == "" || s.Key == "" || s.Actor == "" {
		return nil, fmt.Errorf("kind, idempotency key and actor are required")
	}
	seen := map[string]bool{}
	for _, item := range s.Items {
		if item.Key == "" || seen[item.Key] {
			return nil, fmt.Errorf("item keys must be non-empty and unique")
		}
		seen[item.Key] = true
	}
	raw, err := json.Marshal(struct {
		Ref      string
		Items    []Input
		Deadline time.Time
	}{s.InputRef, s.Items, s.Deadline})
	if err != nil {
		return nil, err
	}
	if len(raw) > 8<<20 || len(s.Items) > 10000 {
		return nil, fmt.Errorf("workflow input exceeds submission budget; use an input manifest")
	}
	digest := sha256.Sum256(raw)
	identity := sha256.Sum256([]byte(s.Kind + "\x00" + s.Actor + "\x00" + s.Key))
	var id primitive.ObjectID
	copy(id[:], identity[:12])
	_, err = r.DB.Collection("workflow_capacity").UpdateOne(ctx, bson.M{"_id": s.Kind}, bson.M{"$setOnInsert": bson.M{"active": 0}}, options.Update().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return nil, err
	}
	var out *Job
	err = r.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		existing, e := r.Get(sc, id)
		if e == nil {
			if existing.Digest != hex.EncodeToString(digest[:]) {
				return ErrConflict
			}
			out = existing
			return nil
		}
		if !errors.Is(e, mongo.ErrNoDocuments) {
			return e
		}
		result, e := r.DB.Collection("workflow_capacity").UpdateOne(sc, bson.M{"_id": s.Kind, "active": bson.M{"$lt": r.Capacity}}, bson.M{"$inc": bson.M{"active": 1}})
		if e != nil {
			return e
		}
		if result.MatchedCount == 0 {
			return ErrCapacity
		}
		now := time.Now().UTC()
		deadline := s.Deadline
		if deadline.IsZero() {
			deadline = now.Add(24 * time.Hour)
			if s.Kind == "delivery_preview" || s.Kind == "delivery_recovery" {
				deadline = now.Add(5 * time.Minute)
			}
		}
		if !deadline.After(now) {
			return fmt.Errorf("workflow deadline has expired")
		}
		job := Job{ID: id, Schema: 1, Kind: s.Kind, Actor: s.Actor, InputRef: s.InputRef, Digest: hex.EncodeToString(digest[:]), State: "queued", CreatedAt: now, Deadline: deadline, Total: len(s.Items)}
		if _, e := r.DB.Collection("workflow_jobs").InsertOne(sc, job); e != nil {
			return e
		}
		for _, input := range s.Items {
			item := Item{ID: id.Hex() + ":" + input.Key, JobID: id, Key: input.Key, Input: input.Data}
			if _, e := r.DB.Collection("workflow_items").InsertOne(sc, item); e != nil {
				return e
			}
		}
		if e := r.event(sc, job, "queued", ""); e != nil {
			return e
		}
		out = &job
		return nil
	})
	return out, err
}
func (r *Repository) Get(ctx context.Context, id primitive.ObjectID) (*Job, error) {
	var j Job
	err := r.DB.Collection("workflow_jobs").FindOne(ctx, bson.M{"_id": id}).Decode(&j)
	return &j, err
}
func (r *Repository) Claim(ctx context.Context, kind, owner string, lease time.Duration) (*Job, error) {
	if owner == "" || lease <= 0 || lease > 5*time.Minute {
		return nil, fmt.Errorf("invalid lease")
	}
	now := time.Now()
	var j Job
	err := r.DB.Collection("workflow_jobs").FindOneAndUpdate(ctx, bson.M{"kind": kind, "cancel_requested": false, "deadline_at": bson.M{"$gt": now}, "$and": bson.A{bson.M{"$or": bson.A{bson.M{"next_run_at": bson.M{"$exists": false}}, bson.M{"next_run_at": bson.M{"$lte": now}}}}}, "$or": bson.A{bson.M{"state": "queued"}, bson.M{"state": "running", "lease_until": bson.M{"$lte": now}}}}, bson.M{"$set": bson.M{"state": "running", "owner": owner, "lease_until": now.Add(lease)}, "$inc": bson.M{"fence": 1, "attempts": 1}}, options.FindOneAndUpdate().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}).SetReturnDocument(options.After)).Decode(&j)
	return &j, err
}
func leaseFilter(j Job) bson.M {
	return bson.M{"_id": j.ID, "state": "running", "owner": j.Owner, "fence": j.Fence, "lease_until": bson.M{"$gt": time.Now()}, "deadline_at": bson.M{"$gt": time.Now()}, "cancel_requested": false}
}
func (r *Repository) Heartbeat(ctx context.Context, j Job, lease time.Duration) error {
	if lease <= 0 || lease > 5*time.Minute {
		return fmt.Errorf("invalid lease")
	}
	result, err := r.DB.Collection("workflow_jobs").UpdateOne(ctx, leaseFilter(j), bson.M{"$set": bson.M{"lease_until": time.Now().Add(lease)}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrLeaseLost
	}
	return nil
}
func (r *Repository) CommitItem(ctx context.Context, j Job, key string, mutate func(mongo.SessionContext) (bson.M, error)) error {
	return r.CommitStage(ctx, j, key, true, mutate)
}

// CommitStage checkpoints a multi-stage item without declaring an external
// publish/delivery complete before its own idempotent verification has finished.
func (r *Repository) CommitStage(ctx context.Context, j Job, key string, done bool, mutate func(mongo.SessionContext) (bson.M, error)) error {
	return r.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		result, err := r.DB.Collection("workflow_jobs").UpdateOne(sc, leaseFilter(j), bson.M{"$inc": bson.M{"revision": 1}})
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return ErrLeaseLost
		}
		var item Item
		if err := r.DB.Collection("workflow_items").FindOne(sc, bson.M{"_id": j.ID.Hex() + ":" + key}).Decode(&item); err != nil {
			return err
		}
		if item.Done {
			return nil
		}
		output, err := mutate(sc)
		if err != nil {
			return err
		}
		encoded, err := bson.Marshal(output)
		if err != nil {
			return err
		}
		if len(encoded) > 1<<20 {
			return fmt.Errorf("item result exceeds budget")
		}
		if _, err := r.DB.Collection("workflow_items").UpdateOne(sc, bson.M{"_id": item.ID}, bson.M{"$set": bson.M{"done": done, "result": output}}); err != nil {
			return err
		}
		if !done {
			return nil
		}
		_, err = r.DB.Collection("workflow_jobs").UpdateOne(sc, bson.M{"_id": j.ID}, bson.M{"$inc": bson.M{"completed": 1}})
		return err
	})
}
func (r *Repository) Finish(ctx context.Context, j Job, state, code string) error {
	return r.FinishWith(ctx, j, state, code, nil)
}

type Finalizer func(mongo.SessionContext, Job, string, string) error

func (r *Repository) FinishWith(ctx context.Context, j Job, state, code string, finalize Finalizer) error {
	if state != "succeeded" && state != "failed" && state != "needs_review" && state != "partial" {
		return fmt.Errorf("invalid terminal state")
	}
	return r.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		filter := leaseFilter(j)
		if state == "succeeded" {
			filter["$expr"] = bson.M{"$eq": bson.A{"$completed", "$total"}}
		}
		result, err := r.DB.Collection("workflow_jobs").UpdateOne(sc, filter, bson.M{"$set": bson.M{"state": state, "error_code": code, "finished_at": time.Now()}})
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return ErrLeaseLost
		}
		if finalize != nil {
			if err := finalize(sc, j, state, code); err != nil {
				return err
			}
		}
		return r.release(sc, j, state, code)
	})
}

// RetryLocal is for explicit retries of database-only executors. External
// operations must use their own reconciliation protocol, not this shortcut.
func (r *Repository) RetryLocal(ctx context.Context, id primitive.ObjectID) error {
	return r.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		j, err := r.Get(sc, id)
		if err != nil {
			return err
		}
		if j.Kind != "fork_merge" {
			return fmt.Errorf("executor does not support local retry")
		}
		if j.State == "queued" || j.State == "running" {
			return nil
		}
		if j.State != "partial" && j.State != "failed" {
			return fmt.Errorf("workflow cannot be retried from %s", j.State)
		}
		result, err := r.DB.Collection("workflow_capacity").UpdateOne(sc, bson.M{"_id": j.Kind, "active": bson.M{"$lt": r.Capacity}}, bson.M{"$inc": bson.M{"active": 1}})
		if err != nil {
			return err
		}
		if result.MatchedCount == 0 {
			return ErrCapacity
		}
		_, err = r.DB.Collection("workflow_jobs").UpdateOne(sc, bson.M{"_id": id, "state": j.State, "fence": j.Fence}, bson.M{"$set": bson.M{"state": "queued", "cancel_requested": false, "deadline_at": time.Now().Add(24 * time.Hour)}, "$unset": bson.M{"owner": "", "lease_until": "", "next_run_at": "", "error_code": ""}, "$inc": bson.M{"fence": 1}})
		return err
	})
}
func (r *Repository) release(ctx context.Context, j Job, state, code string) error {
	result, err := r.DB.Collection("workflow_capacity").UpdateOne(ctx, bson.M{"_id": j.Kind, "active": bson.M{"$gt": 0}}, bson.M{"$inc": bson.M{"active": -1}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("workflow capacity ledger inconsistent")
	}
	return r.event(ctx, j, state, code)
}
func (r *Repository) event(ctx context.Context, j Job, state, code string) error {
	_, err := r.DB.Collection("workflow_events").InsertOne(ctx, Event{JobID: j.ID, At: time.Now(), State: state, Code: code})
	return err
}
func (r *Repository) Cancel(ctx context.Context, id primitive.ObjectID) error {
	return r.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		j, err := r.Get(sc, id)
		if err != nil {
			return err
		}
		if j.State != "queued" && j.State != "running" {
			return nil
		}
		_, err = r.DB.Collection("workflow_jobs").UpdateOne(sc, bson.M{"_id": id}, bson.M{"$set": bson.M{"state": "cancelled", "cancel_requested": true, "finished_at": time.Now()}, "$inc": bson.M{"fence": 1}})
		if err != nil {
			return err
		}
		return r.release(sc, *j, "cancelled", "")
	})
}
func (r *Repository) Items(ctx context.Context, id primitive.ObjectID, after string, limit int) ([]Item, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	c, err := r.DB.Collection("workflow_items").Find(ctx, bson.M{"job_id": id, "key": bson.M{"$gt": after}}, options.Find().SetSort(bson.D{{Key: "key", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	var items []Item
	err = c.All(ctx, &items)
	return items, err
}
func (r *Repository) List(ctx context.Context, kind string, before primitive.ObjectID, limit int) ([]Job, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	filter := bson.M{}
	if kind != "" {
		filter["kind"] = kind
	}
	if !before.IsZero() {
		anchor, err := r.Get(ctx, before)
		if err != nil {
			return nil, err
		}
		filter["$or"] = bson.A{bson.M{"created_at": bson.M{"$lt": anchor.CreatedAt}}, bson.M{"created_at": anchor.CreatedAt, "_id": bson.M{"$lt": before}}}
	}
	c, err := r.DB.Collection("workflow_jobs").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	var rows []Job
	err = c.All(ctx, &rows)
	return rows, err
}
