package staticdelivery

import (
	"context"
	"strings"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// These writes serialize local configuration removal with child creation and
// queued work. Call them inside the same transaction as the dependent write.
func (r *Repository) touchManagementRecord(ctx context.Context, collection string, id primitive.ObjectID) error {
	result, err := r.collection(collection).UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$inc": bson.M{"management_revision": int64(1)}})
	if err == nil && result.MatchedCount != 1 {
		return failure(CodeNotFound, "configuration no longer exists", nil)
	}
	return err
}

func (r *Repository) managementIdle(ctx context.Context, t Target) error {
	if !t.ActiveJobID.IsZero() || t.Status == "initializing" {
		return failure(CodeTargetBusy, "Worker has an active operation", nil)
	}
	n, err := r.collection("jobs").CountDocuments(ctx, bson.M{"target_id": t.ID, "state": bson.M{"$nin": bson.A{JobSucceeded, JobFailed, JobCancelled}}})
	if err != nil {
		return err
	}
	if n != 0 {
		return failure(CodeTargetBusy, "Worker has queued or unresolved publishing work", nil)
	}
	n, err = r.db.Collection("workflow_jobs").CountDocuments(ctx, bson.M{"kind": bson.M{"$in": bson.A{"delivery_preview", "delivery_recovery"}}, "input_ref": t.ID.Hex(), "state": bson.M{"$in": bson.A{"queued", "running", "needs_review"}}})
	if err != nil {
		return err
	}
	if n != 0 {
		return failure(CodeTargetBusy, "Worker has a pending preview or recovery", nil)
	}
	return nil
}

// Delete removes only local configuration and credentials. Historical task and
// artifact records remain subject to their original retention policies.
func (s *AccountService) Delete(ctx context.Context, id primitive.ObjectID) error {
	return s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, err := s.Get(sc, id); err != nil {
			return err
		}
		if err := s.repo.touchManagementRecord(sc, "accounts", id); err != nil {
			return err
		}
		cur, err := s.repo.collection("targets").Find(sc, bson.M{"account_ref": id})
		if err != nil {
			return err
		}
		defer cur.Close(sc)
		var targets []Target
		if err := cur.All(sc, &targets); err != nil {
			return err
		}
		for _, target := range targets {
			if err := s.repo.managementIdle(sc, target); err != nil {
				return err
			}
			if s.repo.references != nil {
				if err := s.repo.references.ReleaseOwner(sc, "delivery-current:"+target.ID.Hex()); err != nil {
					return err
				}
			}
		}
		if _, err := s.repo.collection("targets").DeleteMany(sc, bson.M{"account_ref": id}); err != nil {
			return err
		}
		if _, err := s.repo.collection("credentials").DeleteMany(sc, bson.M{"account_ref": id}); err != nil {
			return err
		}
		_, err = s.repo.collection("accounts").DeleteOne(sc, bson.M{"_id": id})
		return err
	})
}

func (s *TargetService) Delete(ctx context.Context, id, account primitive.ObjectID) error {
	return s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		t, err := s.repo.GetTarget(sc, id)
		if err != nil {
			return err
		}
		if t.AccountRef != account {
			return failure(CodeNotFound, "Worker is not in this account", nil)
		}
		if err := s.repo.touchManagementRecord(sc, "accounts", account); err != nil {
			return err
		}
		if err := s.repo.managementIdle(sc, t); err != nil {
			return err
		}
		if s.repo.references != nil {
			if err := s.repo.references.ReleaseOwner(sc, "delivery-current:"+t.ID.Hex()); err != nil {
				return err
			}
		}
		result, err := s.repo.collection("targets").DeleteOne(sc, bson.M{"_id": id, "account_ref": account, "config_revision": t.ConfigRevision})
		if err == nil && result.DeletedCount != 1 {
			return failure(CodeRevisionConflict, "Worker changed", nil)
		}
		return err
	})
}

type TargetUpdate struct {
	AccountRef                            primitive.ObjectID
	ExpectedConfigRevision                int64
	ExpectedManagementRevision            int64
	Name, WorkerName, Environment, Origin string
}

func (s *TargetService) Update(ctx context.Context, id primitive.ObjectID, req TargetUpdate) (Target, error) {
	req.Name, req.WorkerName, req.Origin = strings.TrimSpace(req.Name), strings.TrimSpace(req.WorkerName), strings.TrimSpace(req.Origin)
	if req.AccountRef.IsZero() || req.ExpectedConfigRevision < 1 || req.ExpectedManagementRevision < 0 || req.Name == "" || len(req.Name) > 200 || !cfWorkerName.MatchString(req.WorkerName) || (req.Environment != "test" && req.Environment != "production") {
		return Target{}, failure(CodeInvalidRequest, "invalid Worker configuration", nil)
	}
	var err error
	if req.Origin != "" {
		req.Origin, err = NormalizeOrigin(req.Origin)
		if err != nil {
			return Target{}, err
		}
	}
	err = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		t, err := s.repo.GetTarget(sc, id)
		if err != nil {
			return err
		}
		if t.AccountRef != req.AccountRef {
			return failure(CodeNotFound, "Worker is not in this account", nil)
		}
		if err := s.repo.managementIdle(sc, t); err != nil {
			return err
		}
		if t.ConfigRevision != req.ExpectedConfigRevision || t.ManagementRevision != req.ExpectedManagementRevision {
			return failure(CodeRevisionConflict, "Worker configuration changed; refresh before editing", nil)
		}
		if t.Status == "archived" {
			return failure(CodeTargetNotReady, "restore Worker before editing", nil)
		}
		if t.Status != "draft" && req.WorkerName != t.WorkerName {
			return failure(CodeInvalidRequest, "initialized Worker name is fixed", nil)
		}
		configChanged := req.Origin != t.Origin || req.Environment != t.Environment || req.WorkerName != t.WorkerName
		if configChanged && (t.CurrentRevision > 0 || !t.CurrentReleaseID.IsZero()) {
			return failure(CodeInvalidRequest, "published address and environment changes require a verified deployment", nil)
		}
		if err := s.repo.touchManagementRecord(sc, "accounts", req.AccountRef); err != nil {
			return err
		}
		state := t.Status
		if configChanged && state != "draft" {
			state = "disabled"
		}
		filter := bson.M{"_id": id, "account_ref": req.AccountRef, "config_revision": req.ExpectedConfigRevision, "management_revision": req.ExpectedManagementRevision}
		if req.ExpectedManagementRevision == 0 {
			filter["management_revision"] = bson.M{"$in": bson.A{nil, int64(0)}}
		}
		increment := bson.M{"management_revision": int64(1)}
		if configChanged {
			increment["config_revision"] = int64(1)
		}
		result, err := s.repo.collection("targets").UpdateOne(sc, filter, bson.M{"$set": bson.M{"name": req.Name, "worker_name": req.WorkerName, "environment": req.Environment, "public_origin": req.Origin, "status": state, "updated_at": time.Now().UTC()}, "$inc": increment})
		if err == nil && result.MatchedCount != 1 {
			return failure(CodeRevisionConflict, "Worker changed", nil)
		}
		return err
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return Target{}, failure(CodeTargetExists, "Worker name already has a configuration in this account", nil)
		}
		return Target{}, err
	}
	return s.repo.GetTarget(ctx, id)
}

func (s *CommandService) submitManagedWorkflow(ctx context.Context, id primitive.ObjectID, submission jobs.Submission) (*jobs.Job, error) {
	var job *jobs.Job
	err := s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if err := s.repo.touchManagementRecord(sc, "targets", id); err != nil {
			return err
		}
		var err error
		job, err = s.previewRunner.Repo.Submit(sc, submission)
		return err
	})
	return job, err
}
