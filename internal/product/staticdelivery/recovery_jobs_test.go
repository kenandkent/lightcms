package staticdelivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func recoveryQueryService(t *testing.T, repo *Repository) *CommandService {
	t.Helper()
	svc := NewCommandService(repo, CommandOptions{
		Store:     &memoryReleaseStore{},
		Authorize: func(context.Context, ActorSnapshot, bool, bool) error { return nil },
		InspectTarget: func(context.Context, Target) (RemoteState, error) {
			t.Fatal("reading recovery task metadata must not inspect the remote Worker")
			return RemoteState{}, nil
		},
		VerifyRecovery: func(context.Context, ReleaseManifest) error {
			t.Fatal("reading recovery task metadata must not rerun recovery")
			return nil
		},
	})
	svc.EnablePreviewJobs(jobs.NewRunner(jobs.NewRepository(repo.db, 10), 1))
	return svc
}

func TestRecoveryJobResultCarriesAuthorizedWorkerIdentity(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	svc := recoveryQueryService(t, repo)
	actor := ActorSnapshot{UserID: "admin-one", Owner: "admin-one", Role: "admin"}
	for _, state := range []string{"queued", "running", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			targetID, jobID, deliveryID := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
			// A task keeps its Worker identity even when local configuration has
			// been removed; the authorized workflow's InputRef is the source.
			job := jobs.Job{ID: jobID, Kind: "delivery_recovery", Actor: "another-admin", InputRef: targetID.Hex(), State: state}
			if _, err := repo.db.Collection("workflow_jobs").InsertOne(ctx, job); err != nil {
				t.Fatal(err)
			}
			if state == "succeeded" {
				raw, err := bson.Marshal(previewExecution{Actor: actor, Target: targetID, RecoveryDigest: "saved-proof"})
				if err != nil {
					t.Fatal(err)
				}
				item := jobs.Item{ID: jobID.Hex() + ":prepare", JobID: jobID, Key: "prepare", Done: true, Input: bson.M{"execution": raw}, Result: bson.M{"delivery_job_id": deliveryID}}
				if _, err := repo.db.Collection("workflow_items").InsertOne(ctx, item); err != nil {
					t.Fatal(err)
				}
			}
			result, err := svc.GetRecoveryJob(ctx, actor, jobID)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["target_id"] != targetID.Hex() || wire["job_id"] != jobID.Hex() || wire["state"] != state {
				t.Fatalf("recovery links cannot discover their authorized Worker: %s", data)
			}
			if state == "succeeded" && wire["delivery_job_id"] != deliveryID.Hex() {
				t.Fatalf("completed recovery lost its verified deployment link: %s", data)
			}
		})
	}
}

func TestRecoveryJobRejectsUnauthorizedTargetDiscovery(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	for _, tc := range []struct {
		name, kind string
		actor      ActorSnapshot
		deny       bool
	}{
		{name: "non-admin", kind: "delivery_recovery", actor: ActorSnapshot{UserID: "editor", Owner: "editor", Role: "editor"}},
		{name: "unauthenticated", kind: "delivery_recovery", actor: ActorSnapshot{Role: "admin"}},
		{name: "wrong-workflow-kind", kind: "delivery_preview", actor: ActorSnapshot{UserID: "admin", Owner: "admin", Role: "admin"}},
		{name: "authorization-revoked", kind: "delivery_recovery", actor: ActorSnapshot{UserID: "admin", Owner: "admin", Role: "admin"}, deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targetID, jobID := primitive.NewObjectID(), primitive.NewObjectID()
			if _, err := repo.db.Collection("workflow_jobs").InsertOne(ctx, jobs.Job{ID: jobID, Kind: tc.kind, Actor: "admin", InputRef: targetID.Hex(), State: "queued"}); err != nil {
				t.Fatal(err)
			}
			svc := recoveryQueryService(t, repo)
			if tc.deny {
				svc.options.Authorize = func(context.Context, ActorSnapshot, bool, bool) error {
					return failure(CodePermissionDenied, "authorization revoked", nil)
				}
			}
			result, err := svc.GetRecoveryJob(ctx, tc.actor, jobID)
			if CodeOf(err) != CodePermissionDenied {
				t.Fatalf("unauthorized recovery metadata read: %v", err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["target_id"] == targetID.Hex() {
				t.Fatalf("rejected recovery query disclosed its Worker: %s", data)
			}
		})
	}
}
