package staticdelivery

import (
	"context"
	"encoding/json"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"net/url"
	"sort"
)

type TargetOption struct {
	ID             primitive.ObjectID `json:"id"`
	Name           string             `json:"name"`
	AccountRef     primitive.ObjectID `json:"account_ref"`
	AccountName    string             `json:"account_name"`
	Origin         string             `json:"origin"`
	Environment    string             `json:"environment"`
	Revision       int64              `json:"revision"`
	ConfigRevision int64              `json:"config_revision"`
	Busy           bool               `json:"busy"`
}

func (s *CommandService) History(ctx context.Context, a ActorSnapshot, target primitive.ObjectID, offset, limit int64) ([]Job, error) {
	if e := s.authorize(ctx, a, true, false); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, failure(CodeInvalidRequest, "invalid history pagination", nil)
	}
	filter := bson.M{"target_id": target}
	if a.Role != "admin" {
		filter["actor.owner"] = a.Owner
	}
	cur, e := s.repo.collection("jobs").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetSkip(offset).SetLimit(limit))
	if e != nil {
		return nil, e
	}
	defer cur.Close(ctx)
	out := []Job{}
	e = cur.All(ctx, &out)
	return out, e
}
func (s *CommandService) Cancel(ctx context.Context, a ActorSnapshot, id primitive.ObjectID, key string) (Job, error) {
	j, e := s.GetJob(ctx, a, id)
	if e != nil {
		return j, e
	}
	if j.Kind != "export" && j.Kind != "deploy" {
		return j, failure(CodePermissionDenied, "this operation requires its dedicated management workflow", nil)
	}
	body, _ := json.Marshal(map[string]string{"job_id": id.Hex()})
	_, _, e = s.repo.ReserveOperation(ctx, OperationRequest{Owner: a.Owner, Action: "cancel", TargetScope: id.Hex(), Key: key, Body: body})
	if e != nil {
		return j, e
	}
	if e = s.repo.CancelJob(ctx, id); e != nil {
		return j, e
	}
	return s.GetJob(ctx, a, id)
}

func (s *CommandService) Options(ctx context.Context, a ActorSnapshot) ([]TargetOption, error) {
	if e := s.authorize(ctx, a, true, false); e != nil {
		return nil, e
	}
	targets, e := s.options.Accounts.repo.collection("targets").Find(ctx, map[string]any{"status": "ready"})
	if e != nil {
		return nil, e
	}
	defer targets.Close(ctx)
	out := []TargetOption{}
	for targets.Next(ctx) {
		var t Target
		if e = targets.Decode(&t); e != nil {
			return nil, e
		}
		account, e := s.options.Accounts.Get(ctx, t.AccountRef)
		if e != nil || account.Status != "enabled" {
			continue
		}
		out = append(out, TargetOption{t.ID, t.Name, t.AccountRef, account.Name, t.Origin, t.Environment, t.CurrentRevision, t.ConfigRevision, !t.ActiveJobID.IsZero()})
	}
	return out, targets.Err()
}
func (s *CommandService) JobLinks(ctx context.Context, a ActorSnapshot, id primitive.ObjectID) ([]PageChange, error) {
	j, e := s.GetJob(ctx, a, id)
	if e != nil {
		return nil, e
	}
	if j.Kind == "export" || j.State != JobSucceeded {
		return []PageChange{}, nil
	}
	var r ReleaseManifest
	if summaries, ok := s.store.(interface {
		Summary(context.Context, primitive.ObjectID) (ReleaseManifest, error)
	}); ok {
		r, e = summaries.Summary(ctx, j.ReleaseID)
	} else {
		r, e = s.store.Load(ctx, j.ReleaseID)
	}
	if e != nil {
		return nil, e
	}
	out := []PageChange{}
	for path, p := range r.Pages {
		out = append(out, PageChange{Path: path, PublicationID: p.PublicationID, URL: r.Origin + (&url.URL{Path: path}).EscapedPath()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
