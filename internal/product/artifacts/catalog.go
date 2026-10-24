package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"time"
)

type Object struct {
	Hash       string    `bson:"_id" json:"hash"`
	Size       int64     `bson:"size" json:"size"`
	MIME       string    `bson:"mime,omitempty" json:"mime,omitempty"`
	State      string    `bson:"state" json:"state"`
	Generation int64     `bson:"generation" json:"generation"`
	Pinned     bool      `bson:"pinned" json:"pinned"`
	Failed     bool      `bson:"failed" json:"failed"`
	CreatedAt  time.Time `bson:"created_at" json:"created_at"`
	DeletePlan string    `bson:"delete_plan,omitempty" json:"-"`
}
type CleanupPlan struct {
	Digest    string    `bson:"_id" json:"confirmation_digest"`
	Owner     string    `bson:"owner" json:"-"`
	Items     []Object  `bson:"items" json:"items"`
	ExpiresAt time.Time `bson:"expires_at" json:"expires_at"`
}
type reference struct {
	ID        string     `bson:"_id"`
	Owner     string     `bson:"owner"`
	Hash      string     `bson:"hash"`
	ExpiresAt *time.Time `bson:"expires_at,omitempty"`
}
type Catalog struct {
	DB                                 *database.DB
	Store                              *Store
	RetentionDays, FailedRetentionDays int
}

func NewCatalog(db *database.DB, store *Store, retention, failed int) *Catalog {
	if retention < 1 {
		retention = 90
	}
	if failed < 1 {
		failed = 7
	}
	return &Catalog{db, store, retention, failed}
}
func (c *Catalog) EnsureIndexes(ctx context.Context) error {
	for collection, keys := range map[string][]mongo.IndexModel{
		"artifact_objects": {
			{Keys: bson.D{{Key: "state", Value: 1}, {Key: "pinned", Value: 1}, {Key: "created_at", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: 1}}},
		},
		"artifact_refs": {{Keys: bson.D{{Key: "hash", Value: 1}, {Key: "expires_at", Value: 1}}}, {Keys: bson.D{{Key: "owner", Value: 1}}}, {Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0)}},
	} {
		if _, err := c.DB.Collection(collection).Indexes().CreateMany(ctx, keys); err != nil {
			return err
		}
	}
	return nil
}
func (c *Catalog) Register(ctx context.Context, ref Ref, failed bool) error {
	unlock, err := c.Store.lockObject(ctx, ref.Hash)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := objectPath(ref.Hash); err != nil {
		return err
	}
	// Registration never resurrects a deleting object. Retry after its tombstone
	// is committed, then Put recreates the bytes before a new generation starts.
	reader, err := c.Store.Open(ctx, ref)
	if err != nil {
		return err
	}
	reader.Close()
	return c.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var old Object
		err := c.DB.Collection("artifact_objects").FindOne(sc, bson.M{"_id": ref.Hash}).Decode(&old)
		if errors.Is(err, mongo.ErrNoDocuments) {
			_, err := c.DB.Collection("artifact_objects").InsertOne(sc, Object{Hash: ref.Hash, Size: ref.Size, MIME: ref.MIME, State: "available", Generation: 1, Failed: failed, CreatedAt: time.Now()})
			return err
		}
		if err != nil {
			return err
		}
		if old.State == "deleting" {
			return fmt.Errorf("object is being deleted")
		}
		if old.Size != ref.Size {
			return ErrCorrupt
		}
		if old.State == "available" {
			return nil
		}
		_, err = c.DB.Collection("artifact_objects").UpdateOne(sc, bson.M{"_id": ref.Hash, "state": "deleted", "generation": old.Generation}, bson.M{"$set": bson.M{"state": "available", "mime": ref.MIME, "failed": failed, "created_at": time.Now()}, "$inc": bson.M{"generation": 1}, "$unset": bson.M{"delete_plan": "", "deleted_at": ""}})
		return err
	})
}
func refID(owner, hash string) string {
	digest := sha256.Sum256([]byte(owner + "\x00" + hash))
	return hex.EncodeToString(digest[:])
}
func (c *Catalog) Reference(ctx context.Context, owner string, ref Ref, expires *time.Time) error {
	if owner == "" {
		return fmt.Errorf("reference owner required")
	}
	if expires != nil && !expires.After(time.Now()) {
		return fmt.Errorf("reference already expired")
	}
	return c.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		result, err := c.DB.Collection("artifact_objects").UpdateOne(sc, bson.M{"_id": ref.Hash, "state": "available", "size": ref.Size}, bson.M{"$inc": bson.M{"reference_revision": 1}})
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return fmt.Errorf("object unavailable for reference")
		}
		id := refID(owner, ref.Hash)
		var old reference
		err = c.DB.Collection("artifact_refs").FindOne(sc, bson.M{"_id": id}).Decode(&old)
		if err == nil && (old.ExpiresAt == nil || (expires != nil && old.ExpiresAt.After(*expires))) {
			expires = old.ExpiresAt
		} else if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return err
		}
		_, err = c.DB.Collection("artifact_refs").ReplaceOne(sc, bson.M{"_id": id}, reference{ID: id, Owner: owner, Hash: ref.Hash, ExpiresAt: expires}, options.Replace().SetUpsert(true))
		return err
	})
}
func (c *Catalog) ReleaseOwner(ctx context.Context, owner string) error {
	if owner == "" {
		return fmt.Errorf("reference owner required")
	}
	return c.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		cursor, err := c.DB.Collection("artifact_refs").Find(sc, bson.M{"owner": owner})
		if err != nil {
			return err
		}
		defer cursor.Close(sc)
		for cursor.Next(sc) {
			var ref reference
			if err := cursor.Decode(&ref); err != nil {
				return err
			}
			if _, err := c.DB.Collection("artifact_objects").UpdateOne(sc, bson.M{"_id": ref.Hash}, bson.M{"$inc": bson.M{"reference_revision": 1}}); err != nil {
				return err
			}
		}
		if err := cursor.Err(); err != nil {
			return err
		}
		_, err = c.DB.Collection("artifact_refs").DeleteMany(sc, bson.M{"owner": owner})
		return err
	})
}
func (c *Catalog) Pin(ctx context.Context, hash string, pinned bool) error {
	result, err := c.DB.Collection("artifact_objects").UpdateOne(ctx, bson.M{"_id": hash, "state": "available"}, bson.M{"$set": bson.M{"pinned": pinned}, "$inc": bson.M{"reference_revision": 1}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("object unavailable")
	}
	return nil
}
func (c *Catalog) referenced(ctx context.Context, hash string) (bool, error) {
	n, err := c.DB.Collection("artifact_refs").CountDocuments(ctx, bson.M{"hash": hash, "$or": bson.A{bson.M{"expires_at": bson.M{"$exists": false}}, bson.M{"expires_at": nil}, bson.M{"expires_at": bson.M{"$gt": time.Now()}}}})
	return n > 0, err
}
func (c *Catalog) eligible(object Object) bool {
	days := c.RetentionDays
	if object.Failed {
		days = c.FailedRetentionDays
	}
	return object.State == "available" && !object.Pinned && object.CreatedAt.Add(time.Duration(days)*24*time.Hour).Before(time.Now())
}
func (c *Catalog) PreviewCleanup(ctx context.Context, owner string) (CleanupPlan, error) {
	plan := CleanupPlan{Owner: owner, Items: []Object{}, ExpiresAt: time.Now().Add(15 * time.Minute)}
	if owner == "" {
		return plan, fmt.Errorf("cleanup owner required")
	}
	cursor, err := c.DB.Collection("artifact_objects").Find(ctx, bson.M{"state": "available", "pinned": false}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}).SetLimit(1000))
	if err != nil {
		return plan, err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var object Object
		if err := cursor.Decode(&object); err != nil {
			return plan, err
		}
		if !c.eligible(object) {
			continue
		}
		active, err := c.referenced(ctx, object.Hash)
		if err != nil {
			return plan, err
		}
		if !active {
			plan.Items = append(plan.Items, object)
		}
		if len(plan.Items) == 100 {
			break
		}
	}
	if err := cursor.Err(); err != nil {
		return plan, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	digest := sha256.Sum256(raw)
	plan.Digest = hex.EncodeToString(digest[:])
	_, err = c.DB.Collection("artifact_cleanup_plans").InsertOne(ctx, plan)
	return plan, err
}
func (c *Catalog) ConfirmCleanup(ctx context.Context, owner, digest string, confirmed bool) error {
	if !confirmed {
		return fmt.Errorf("explicit cleanup confirmation required")
	}
	var plan CleanupPlan
	if err := c.DB.Collection("artifact_cleanup_plans").FindOne(ctx, bson.M{"_id": digest, "owner": owner, "expires_at": bson.M{"$gt": time.Now()}}).Decode(&plan); err != nil {
		return err
	}
	for _, candidate := range plan.Items {
		if err := c.deleteCandidate(ctx, plan, candidate); err != nil {
			return err
		}
	}
	return nil
}

// ResumeCleanup finishes only deletion intents already fenced by this owner.
// Expiry never authorizes deleting additional objects from an old preview.
func (c *Catalog) ResumeCleanup(ctx context.Context, owner, digest string, confirmed bool) error {
	if !confirmed || owner == "" {
		return fmt.Errorf("explicit cleanup confirmation required")
	}
	var plan CleanupPlan
	if err := c.DB.Collection("artifact_cleanup_plans").FindOne(ctx, bson.M{"_id": digest, "owner": owner}).Decode(&plan); err != nil {
		return err
	}
	for _, candidate := range plan.Items {
		var object Object
		if err := c.DB.Collection("artifact_objects").FindOne(ctx, bson.M{"_id": candidate.Hash}).Decode(&object); err != nil {
			return err
		}
		if object.DeletePlan != digest || object.Generation != candidate.Generation || (object.State != "deleting" && object.State != "deleted") {
			continue
		}
		if err := c.deleteCandidate(ctx, plan, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) deleteCandidate(ctx context.Context, plan CleanupPlan, candidate Object) error {
	unlock, lockErr := c.Store.lockObject(ctx, candidate.Hash)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	digest := plan.Digest
	alreadyDeleted := false
	err := c.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var object Object
		if err := c.DB.Collection("artifact_objects").FindOne(sc, bson.M{"_id": candidate.Hash}).Decode(&object); err != nil {
			return err
		}
		if object.Generation != candidate.Generation {
			return fmt.Errorf("object generation changed; preview again")
		}
		if object.State == "deleted" && object.DeletePlan == digest {
			alreadyDeleted = true
			return nil
		}
		if object.State == "deleting" && object.DeletePlan == digest {
			return nil
		}
		if !c.eligible(object) {
			return fmt.Errorf("object no longer eligible")
		}
		active, err := c.referenced(sc, object.Hash)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf("object acquired a protected reference; preview again")
		}
		result, err := c.DB.Collection("artifact_objects").UpdateOne(sc, bson.M{"_id": object.Hash, "generation": object.Generation, "state": "available", "pinned": false}, bson.M{"$set": bson.M{"state": "deleting", "delete_plan": digest}, "$inc": bson.M{"reference_revision": 1}})
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return fmt.Errorf("cleanup lost object fence")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if alreadyDeleted {
		return nil
	}
	if err := c.Store.Remove(candidate.Hash); err != nil {
		return err
	}
	result, err := c.DB.Collection("artifact_objects").UpdateOne(ctx, bson.M{"_id": candidate.Hash, "generation": candidate.Generation, "state": "deleting", "delete_plan": digest}, bson.M{"$set": bson.M{"state": "deleted", "deleted_at": time.Now()}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		var done Object
		if err := c.DB.Collection("artifact_objects").FindOne(ctx, bson.M{"_id": candidate.Hash}).Decode(&done); err != nil {
			return err
		}
		if done.State != "deleted" || done.DeletePlan != digest {
			return fmt.Errorf("cleanup tombstone changed")
		}
	}
	return nil
}
