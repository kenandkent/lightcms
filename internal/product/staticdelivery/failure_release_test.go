package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
	"time"
)

func TestPreActivationFailureReleasesOnlyItsOwnTarget(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign"}[foreign], func(t *testing.T) {
			repo, ctx := connectDeliveryTestDB(t)
			target := releaseTarget()
			target.Status = "ready"
			job := Job{Record: newRecord(), State: JobUploading, Kind: "deploy", TargetID: target.ID, LeaseOwner: "runner"}
			target.ActiveJobID = job.ID
			if foreign {
				target.ActiveJobID = primitive.NewObjectID()
			}
			repo.collection("targets").InsertOne(ctx, target)
			repo.collection("jobs").InsertOne(ctx, job)
			runner := NewRunner(repo, RunnerDependencies{})
			if err := runner.block(ctx, &job, failure(CodeVersionUploadFailed, "rejected", nil)); err != nil {
				t.Fatal(err)
			}
			var got Job
			repo.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
			latest, _ := repo.GetTarget(ctx, target.ID)
			if got.State != JobFailed || got.ErrorCode != CodeVersionUploadFailed {
				t.Fatalf("job=%+v", got)
			}
			if !foreign && !latest.ActiveJobID.IsZero() || foreign && latest.ActiveJobID != target.ActiveJobID {
				t.Fatal("incorrect target owner release")
			}
			if !foreign {
				if err := repo.ClaimTarget(ctx, target.ID, primitive.NewObjectID(), target.CurrentRevision); err != nil {
					t.Fatalf("next deployment still blocked: %v", err)
				}
			}
		})
	}
}

func TestFailureAfterActivationIntentKeepsOwner(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	now := time.Now().UTC()
	target := releaseTarget()
	target.Status = "ready"
	job := Job{Record: newRecord(), State: JobActivating, Kind: "deploy", TargetID: target.ID, LeaseOwner: "runner", Checkpoint: Checkpoint{ActivationIntentAt: &now}}
	target.ActiveJobID = job.ID
	repo.collection("targets").InsertOne(ctx, target)
	repo.collection("jobs").InsertOne(ctx, job)
	runner := NewRunner(repo, RunnerDependencies{})
	if err := runner.block(context.WithoutCancel(ctx), &job, failure(CodeCFUnavailable, "timeout", nil)); err != nil {
		t.Fatal(err)
	}
	latest, _ := repo.GetTarget(ctx, target.ID)
	if job.State != JobActivationUnknown || latest.ActiveJobID != job.ID {
		t.Fatal("uncertain activation released")
	}
}

func TestLegacyBlockedFailureRecovery(t *testing.T) {
	for _, scenario := range []string{"expired", "live-lease", "activation-intent"} {
		t.Run(scenario, func(t *testing.T) {
			repo, ctx := connectDeliveryTestDB(t)
			now := time.Now().UTC()
			target := releaseTarget()
			target.Status = "ready"
			job := Job{Record: newRecord(), State: JobBlocked, Kind: "deploy", TargetID: target.ID, ErrorCode: CodeVersionUploadFailed, LeaseUntil: now.Add(-time.Minute)}
			if scenario == "live-lease" {
				job.LeaseUntil = now.Add(time.Minute)
			}
			if scenario == "activation-intent" {
				job.Checkpoint.ActivationIntentAt = &now
			}
			target.ActiveJobID = job.ID
			repo.collection("targets").InsertOne(ctx, target)
			repo.collection("jobs").InsertOne(ctx, job)
			if e := repo.releaseLegacyFailures(ctx); e != nil {
				t.Fatal(e)
			}
			latest, _ := repo.GetTarget(ctx, target.ID)
			var got Job
			repo.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got)
			if scenario == "expired" {
				if got.State != JobFailed || !latest.ActiveJobID.IsZero() || got.ErrorCode != job.ErrorCode {
					t.Fatal("legacy failure still owns target")
				}
			} else if got.State != JobBlocked || latest.ActiveJobID != job.ID {
				t.Fatal("unsafe recovery")
			}
		})
	}
}
