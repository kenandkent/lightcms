package staticdelivery

import (
	"context"
	"errors"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type previewExecution struct {
	RecoveryDigest string
	Actor          ActorSnapshot
	Target         primitive.ObjectID
	Request        DeployRequest
}
type PreviewJobResult struct {
	TargetID  primitive.ObjectID `json:"target_id"`
	JobID     primitive.ObjectID `json:"job_id"`
	State     string             `json:"state"`
	ErrorCode string             `json:"error_code,omitempty"`
	Preview   *DeploymentPreview `json:"preview,omitempty"`
}

func (s *CommandService) EnablePreviewJobs(runner *jobs.Runner) {
	s.previewRunner = runner
	s.enableRecoveryJobs(runner)
	runner.Register("delivery_preview", jobs.Executor{
		Concurrency: 1,
		Authorize: func(ctx context.Context, j jobs.Job) error {
			x, _, err := s.previewInput(ctx, j)
			if err != nil {
				return err
			}
			return s.authorize(ctx, x.Actor, true, false)
		},
		Run: func(ctx context.Context, j jobs.Job) error {
			x, item, err := s.previewInput(ctx, j)
			if err != nil {
				return err
			}
			if item.Done {
				return nil
			}
			preview, err := s.PreviewDeployment(ctx, x.Actor, x.Target, x.Request)
			if err != nil {
				code := CodeOf(err)
				if code == "" {
					code = "PREVIEW_PREPARATION_FAILED"
				}
				return &jobs.ExecutionError{Code: code, Cause: err}
			}
			data, err := bson.Marshal(preview)
			if err != nil {
				return err
			}
			var result bson.M
			if err := bson.Unmarshal(data, &result); err != nil {
				return err
			}
			return runner.Repo.CommitItem(ctx, j, item.Key, func(mongo.SessionContext) (bson.M, error) { return result, nil })
		},
	})
}
func (s *CommandService) AsyncPreviewAvailable() bool { return s.previewRunner != nil }
func (s *CommandService) previewInput(ctx context.Context, j jobs.Job) (previewExecution, jobs.Item, error) {
	var x previewExecution
	items, err := s.previewRunner.Repo.Items(ctx, j.ID, "", 1)
	if err != nil {
		return x, jobs.Item{}, err
	}
	if len(items) != 1 {
		return x, jobs.Item{}, fmt.Errorf("preview input missing")
	}
	binary, ok := items[0].Input["execution"].(primitive.Binary)
	if !ok {
		return x, items[0], fmt.Errorf("invalid preview snapshot")
	}
	err = bson.Unmarshal(binary.Data, &x)
	return x, items[0], err
}
func (s *CommandService) SubmitPreview(ctx context.Context, a ActorSnapshot, key string, target primitive.ObjectID, req DeployRequest) (PreviewJobResult, error) {
	var result PreviewJobResult
	if s.previewRunner == nil {
		return result, failure(CodeModuleUnavailable, "asynchronous preview unavailable", nil)
	}
	if key == "" {
		return result, failure(CodeIdempotencyRequired, "preview idempotency key is required", nil)
	}
	if err := s.authorize(ctx, a, true, false); err != nil {
		return result, err
	}
	if err := validateDeployment(req); err != nil {
		return result, err
	}
	if _, err := s.availableTarget(ctx, target, req); err != nil {
		return result, err
	}
	encoded, err := bson.Marshal(previewExecution{Actor: a, Target: target, Request: req})
	if err != nil {
		return result, err
	}
	j, err := s.previewRunner.Repo.Submit(ctx, jobs.Submission{Kind: "delivery_preview", Actor: a.Owner, Key: key, InputRef: target.Hex(), Items: []jobs.Input{{Key: "prepare", Data: bson.M{"execution": encoded}}}})
	if err != nil {
		return result, workflowAPIError(err)
	}
	s.previewRunner.Wake()
	return s.GetPreviewJob(ctx, a, j.ID)
}
func (s *CommandService) GetPreviewJob(ctx context.Context, a ActorSnapshot, id primitive.ObjectID) (PreviewJobResult, error) {
	result := PreviewJobResult{JobID: id}
	if s.previewRunner == nil {
		return result, failure(CodeModuleUnavailable, "asynchronous preview unavailable", nil)
	}
	if err := s.authorize(ctx, a, true, false); err != nil {
		return result, err
	}
	j, err := s.previewRunner.Repo.Get(ctx, id)
	if err != nil {
		return result, workflowAPIError(err)
	}
	if j.Kind != "delivery_preview" || (j.Actor != a.Owner && a.Role != "admin") {
		return result, failure(CodePermissionDenied, "preview job belongs to another actor", nil)
	}
	result.State = j.State
	result.TargetID, _ = primitive.ObjectIDFromHex(j.InputRef)
	result.ErrorCode = j.ErrorCode
	if j.State == "succeeded" {
		_, item, err := s.previewInput(ctx, *j)
		if err != nil {
			return result, err
		}
		raw, err := bson.Marshal(item.Result)
		if err != nil {
			return result, err
		}
		var preview DeploymentPreview
		if err := bson.Unmarshal(raw, &preview); err != nil {
			return result, err
		}
		result.Preview = &preview
	}
	return result, nil
}

func workflowAPIError(err error) error {
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return failure(CodeNotFound, "workflow job not found", err)
	case errors.Is(err, jobs.ErrConflict):
		return failure(CodeIdempotencyConflict, "idempotency key has different input", err)
	case errors.Is(err, jobs.ErrCapacity):
		return failure(CodeRateLimited, "workflow queue is full", err)
	default:
		return err
	}
}
