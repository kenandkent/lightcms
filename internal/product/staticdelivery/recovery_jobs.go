package staticdelivery

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"time"
)

type recoveryCommitFence struct{}
type RecoveryJobResult struct {
	JobID         primitive.ObjectID `json:"job_id"`
	TargetID      primitive.ObjectID `json:"target_id"`
	State         string             `json:"state"`
	ErrorCode     string             `json:"error_code,omitempty"`
	DeliveryJobID primitive.ObjectID `json:"delivery_job_id,omitempty"`
}

func (s *CommandService) enableRecoveryJobs(runner *jobs.Runner) {
	runner.Register("delivery_recovery", jobs.Executor{Concurrency: 1, Authorize: func(ctx context.Context, j jobs.Job) error {
		x, _, err := s.previewInput(ctx, j)
		if err != nil {
			return err
		}
		return s.recoveryAllowed(ctx, x.Actor)
	}, Run: func(ctx context.Context, j jobs.Job) error {
		x, item, err := s.previewInput(ctx, j)
		if err != nil {
			return err
		}
		if item.Done {
			return nil
		}
		commit := func(sc mongo.SessionContext, result Job) error {
			return runner.Repo.CommitItem(sc, j, item.Key, func(mongo.SessionContext) (bson.M, error) { return bson.M{"delivery_job_id": result.ID}, nil })
		}
		ctx = context.WithValue(ctx, recoveryCommitFence{}, commit)
		result, err := s.ConfirmRecovery(ctx, x.Actor, x.Target, x.RecoveryDigest, true)
		if err != nil {
			code := CodeOf(err)
			if code == "" {
				code = "RECOVERY_VERIFICATION_FAILED"
			}
			return &jobs.ExecutionError{Code: code, Cause: err}
		}
		return runner.Repo.CommitItem(ctx, j, item.Key, func(mongo.SessionContext) (bson.M, error) { return bson.M{"delivery_job_id": result.ID}, nil })
	}})
}
func (s *CommandService) SubmitRecovery(ctx context.Context, a ActorSnapshot, id primitive.ObjectID, digest string, confirmed bool) (RecoveryJobResult, error) {
	if err := s.recoveryAllowed(ctx, a); err != nil {
		return RecoveryJobResult{}, err
	}
	if s.previewRunner == nil {
		return RecoveryJobResult{}, failure(CodeModuleUnavailable, "durable recovery unavailable", nil)
	}
	if !confirmed {
		return RecoveryJobResult{}, failure(CodeConfirmationRequired, "explicit recovery confirmation required", nil)
	}
	var proof RecoveryPreview
	if err := s.repo.collection("recovery_previews").FindOne(ctx, bson.M{"_id": digest, "owner": a.Owner, "target._id": id, "expires_at": bson.M{"$gt": time.Now()}}).Decode(&proof); err != nil {
		return RecoveryJobResult{}, failure(CodeConfirmationRequired, "recovery preview unavailable or expired", err)
	}
	raw, err := bson.Marshal(previewExecution{Actor: a, Target: id, RecoveryDigest: digest})
	if err != nil {
		return RecoveryJobResult{}, err
	}
	job, err := s.submitManagedWorkflow(ctx, id, jobs.Submission{Kind: "delivery_recovery", Actor: a.Owner, Key: digest, InputRef: id.Hex(), Items: []jobs.Input{{Key: "prepare", Data: bson.M{"execution": raw}}}})
	if err != nil {
		return RecoveryJobResult{}, workflowAPIError(err)
	}
	s.previewRunner.Wake()
	return s.GetRecoveryJob(ctx, a, job.ID)
}
func (s *CommandService) GetRecoveryJob(ctx context.Context, a ActorSnapshot, id primitive.ObjectID) (RecoveryJobResult, error) {
	result := RecoveryJobResult{JobID: id}
	if err := s.recoveryAllowed(ctx, a); err != nil {
		return result, err
	}
	if s.previewRunner == nil {
		return result, failure(CodeModuleUnavailable, "durable recovery unavailable", nil)
	}
	job, err := s.previewRunner.Repo.Get(ctx, id)
	if err != nil {
		return result, workflowAPIError(err)
	}
	if job.Kind != "delivery_recovery" || job.Actor != a.Owner && a.Role != "admin" {
		return result, failure(CodePermissionDenied, "recovery belongs to another actor", nil)
	}
	result.TargetID, _ = primitive.ObjectIDFromHex(job.InputRef)
	result.State = job.State
	result.ErrorCode = job.ErrorCode
	if job.State == "succeeded" {
		_, item, err := s.previewInput(ctx, *job)
		if err != nil {
			return result, err
		}
		result.DeliveryJobID, _ = item.Result["delivery_job_id"].(primitive.ObjectID)
	}
	return result, nil
}
