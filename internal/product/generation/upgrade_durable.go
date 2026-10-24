package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"time"
)

func (s *Service) EnableDurableUpgrades(runner *jobs.Runner, authorize func(context.Context, Actor) error) {
	s.workflow = runner
	s.workflowAuth = authorize
	runner.Register("template_upgrade", jobs.Executor{
		Finalize: func(ctx mongo.SessionContext, j jobs.Job, state, code string) error {
			id, err := primitive.ObjectIDFromHex(j.InputRef)
			if err != nil {
				return err
			}
			set := bson.M{"execution_state": state, "execution_error": code, "updated_at": s.now()}
			if state != "succeeded" {
				set["status"] = UpgradeJobPartial
			}
			_, err = s.db.Collection(CollectionUpgradeJobs).UpdateOne(ctx, bson.M{"_id": id, "workflow_id": j.ID, "status": bson.M{"$ne": UpgradeJobCompleted}}, bson.M{"$set": set})
			return err
		},
		Authorize: func(ctx context.Context, j jobs.Job) error {
			actor, err := s.upgradeWorkflowActor(ctx, j)
			if err != nil {
				return err
			}
			return authorize(ctx, actor)
		},
		Run: func(ctx context.Context, j jobs.Job) error {
			actor, err := s.upgradeWorkflowActor(ctx, j)
			if err != nil {
				return err
			}
			id, err := primitive.ObjectIDFromHex(j.InputRef)
			if err != nil {
				return err
			}
			result, err := s.runUpgradeJob(ctx, actor, id)
			if CodeOf(err) == CodeUpgradeJobConflict {
				return &jobs.Retry{After: 30 * time.Second, Code: "UPGRADE_LEASE_BUSY"}
			}
			if err != nil {
				return err
			}
			if err := runner.Repo.CommitItem(ctx, j, "run", func(mongo.SessionContext) (bson.M, error) {
				return bson.M{"upgrade_job_id": id.Hex(), "status": string(result.Status)}, nil
			}); err != nil {
				return err
			}
			if result.Status != UpgradeJobCompleted {
				return &jobs.PartialError{}
			}
			return nil
		},
	})
}
func (s *Service) upgradeWorkflowActor(ctx context.Context, j jobs.Job) (Actor, error) {
	var actor Actor
	items, err := s.workflow.Repo.Items(ctx, j.ID, "", 1)
	if err != nil {
		return actor, err
	}
	if len(items) != 1 {
		return actor, fmt.Errorf("upgrade actor snapshot missing")
	}
	raw, _ := items[0].Input["actor"].(string)
	err = json.Unmarshal([]byte(raw), &actor)
	return actor, err
}
func (s *Service) SubmitUpgradeRun(ctx context.Context, actor Actor, id primitive.ObjectID) (*jobs.Job, error) {
	if s.workflow == nil {
		return nil, genErr(CodeInternal, "durable upgrades unavailable", nil)
	}
	if !actor.Authenticated || !actor.Can(ScopeContentEdit) || !actor.Can(ScopeContentPublish) {
		return nil, genErr(CodePermissionDenied, "upgrade run requires content.edit + content.publish", nil)
	}
	if err := s.workflowAuth(ctx, actor); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(actor)
	if err != nil {
		return nil, err
	}
	var out *jobs.Job
	err = s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var binding struct {
			WorkflowID *primitive.ObjectID `bson:"workflow_id"`
			Run        int64               `bson:"workflow_run"`
		}
		if err := s.db.FindOne(sc, CollectionUpgradeJobs, bson.M{"_id": id}, &binding); err != nil {
			return err
		}
		if binding.WorkflowID != nil {
			previous, err := s.workflow.Repo.Get(sc, *binding.WorkflowID)
			if err != nil {
				return err
			}
			if previous.State == "queued" || previous.State == "running" || previous.State == "succeeded" {
				out = previous
				return nil
			}
		}
		out, err = s.workflow.Repo.Submit(sc, jobs.Submission{Kind: "template_upgrade", Key: fmt.Sprintf("%s:%d", id.Hex(), binding.Run+1), Actor: actor.ID, InputRef: id.Hex(), Items: []jobs.Input{{Key: "run", Data: bson.M{"actor": string(raw)}}}})
		if err != nil {
			return err
		}
		return s.db.UpdateOne(sc, CollectionUpgradeJobs, bson.M{"_id": id}, bson.M{"$set": bson.M{"workflow_id": out.ID}, "$inc": bson.M{"workflow_run": 1}})
	})
	if err == nil {
		s.workflow.Wake()
	}
	return out, err
}
func (s *Service) waitUpgradeRun(ctx context.Context, actor Actor, id primitive.ObjectID) (UpgradeJob, error) {
	job, err := s.SubmitUpgradeRun(ctx, actor, id)
	if err != nil {
		return UpgradeJob{}, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := s.workflow.Repo.Get(ctx, job.ID)
		if err != nil {
			return UpgradeJob{}, err
		}
		switch current.State {
		case "succeeded", "partial":
			return s.GetUpgradeJob(ctx, actor, id)
		case "failed", "cancelled", "needs_review":
			return UpgradeJob{}, genErr(CodeUpgradeJobConflict, "upgrade execution "+current.State+": "+current.ErrorCode, nil)
		}
		select {
		case <-ctx.Done():
			return UpgradeJob{}, fmt.Errorf("upgrade workflow %s remains persisted: %w", job.ID.Hex(), ctx.Err())
		case <-ticker.C:
		}
	}
}
