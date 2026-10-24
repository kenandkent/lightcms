package jobs

import (
	"context"
	"errors"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"testing"
	"time"
)

func TestRunnerDelayedRetryPartialAndExplicitLocalResume(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepository(db, 1)
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := repo.Submit(ctx, Submission{Kind: "fork_merge", Key: "partial", Actor: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(repo, 1)
	runs := 0
	finalized := []string{}
	runner.Register("fork_merge", Executor{Authorize: func(context.Context, Job) error { return nil }, Run: func(context.Context, Job) error {
		runs++
		switch runs {
		case 1:
			return &Retry{Code: "WAIT"}
		case 2:
			return &PartialError{}
		}
		return nil
	}, Finalize: func(_ mongo.SessionContext, _ Job, state, _ string) error {
		finalized = append(finalized, state)
		return nil
	}})
	if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Claim(ctx, "fork_merge", "early", time.Minute); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("delayed retry ran early: %v", err)
	}
	if err := db.UpdateOne(ctx, "workflow_jobs", bson.M{"_id": job.ID}, bson.M{"$set": bson.M{"next_run_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
		t.Fatal(err)
	}
	partial, err := repo.Get(ctx, job.ID)
	if err != nil || partial.State != "partial" {
		t.Fatalf("partial lost: %+v %v", partial, err)
	}
	other, err := repo.Submit(ctx, Submission{Kind: "fork_merge", Key: "other", Actor: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RetryLocal(ctx, job.ID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("retry exceeded queue capacity: %v", err)
	}
	if err := repo.Cancel(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.RetryLocal(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
		t.Fatal(err)
	}
	done, err := repo.Get(ctx, job.ID)
	if err != nil || done.State != "succeeded" || len(finalized) != 2 || finalized[0] != "partial" || finalized[1] != "succeeded" {
		t.Fatalf("resume or finalization wrong: %+v %+v %v", done, finalized, err)
	}
	foreign, err := repo.Submit(ctx, Submission{Kind: "external", Key: "no-local-retry", Actor: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RetryLocal(ctx, foreign.ID); err == nil {
		t.Fatal("external work accepted unsafe local retry")
	}
}

func TestRunnerLoopShutdownLeavesLeaseForExplicitRecovery(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewRepository(db, 10)
	job, err := repo.Submit(ctx, Submission{Kind: "stop", Key: "shutdown", Actor: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(repo, 2)
	entered := make(chan struct{}, 1)
	runner.Register("stop", Executor{Authorize: func(context.Context, Job) error { return nil }, Run: func(work context.Context, _ Job) error { entered <- struct{}{}; <-work.Done(); return work.Err() }})
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { runner.Run(work); close(done) }()
	runner.Wake()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
	}
	stored, err := repo.Get(ctx, job.ID)
	if err != nil || stored.State != "running" {
		t.Fatalf("shutdown pretended success: %+v %v", stored, err)
	}
	if err := db.UpdateOne(ctx, "workflow_jobs", bson.M{"_id": job.ID}, bson.M{"$set": bson.M{"deadline_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Expire(ctx, "stop"); err != nil {
		t.Fatal(err)
	}
	stored, err = repo.Get(ctx, job.ID)
	if err != nil || stored.State != "needs_review" {
		t.Fatalf("uncertain interrupted work not visible: %+v %v", stored, err)
	}
	if _, err := repo.List(ctx, "stop", primitive.NilObjectID, 50); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerKindLimitLeavesCapacityForOtherWork(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_events", "workflow_capacity"} {
		defer db.Collection(name).Drop(ctx)
	}
	repo := NewRepository(db, 10)
	runner := NewRunner(repo, 3)
	for _, key := range []string{"one", "two"} {
		if _, err := repo.Submit(ctx, Submission{Kind: "heavy", Key: key, Actor: "user"}); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	defer close(release)
	runner.Register("heavy", Executor{Concurrency: 1, Authorize: func(context.Context, Job) error { return nil }, Run: func(context.Context, Job) error { entered <- struct{}{}; <-release; return nil }})
	done := make(chan error, 2)
	go func() { done <- runner.RunOnce(ctx, "heavy") }()
	<-entered
	go func() { done <- runner.RunOnce(ctx, "heavy") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("same-kind waiter consumed worker capacity")
	}
	select {
	case <-entered:
		t.Fatal("kind concurrency exceeded")
	default:
	}
	// Complete before dropping test collections.
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRevokedActorCannotMutate(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	repo := NewRepository(db, 10)
	j, err := repo.Submit(ctx, Submission{Kind: "review", Key: "revoked", Actor: "removed-user", Items: []Input{{Key: "one", Data: bson.M{"title": "frozen"}}}})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(repo, 1)
	called := false
	runner.Register("review", Executor{Authorize: func(context.Context, Job) error { return errors.New("revoked") }, Run: func(context.Context, Job) error { called = true; return nil }})
	if err := runner.RunOnce(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if called || stored.State != "failed" || stored.ErrorCode != "ACTOR_REVOKED" {
		t.Fatalf("revoked actor executed: %+v", stored)
	}
}

func TestRunnerExpiryFinalizesProjectionOnce(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events", "workflow_projection"} {
		defer db.Collection(name).Drop(ctx)
	}
	repo := NewRepository(db, 10)
	job, err := repo.Submit(ctx, Submission{Kind: "expiry", Key: "one", Actor: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Collection("workflow_jobs").UpdateOne(ctx, bson.M{"_id": job.ID}, bson.M{"$set": bson.M{"deadline_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(repo, 1)
	runner.Register("expiry", Executor{Authorize: func(context.Context, Job) error { return nil }, Run: func(context.Context, Job) error { t.Error("expired job ran"); return nil }, Finalize: func(sc mongo.SessionContext, j Job, state, code string) error {
		_, err := db.InsertOne(sc, "workflow_projection", bson.M{"job": j.ID, "state": state, "code": code})
		return err
	}})
	for n := 0; n < 2; n++ {
		if err := runner.RunOnce(ctx, "expiry"); err != nil {
			t.Fatal(err)
		}
	}
	count, err := db.Count(ctx, "workflow_projection", bson.M{"job": job.ID, "state": "failed", "code": "DEADLINE_EXCEEDED"})
	if err != nil || count != 1 {
		t.Fatalf("expiry projection not committed exactly once: %d %v", count, err)
	}
}

func TestRunnerResumesFrozenInputAfterRestart(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events", "workflow_output"} {
		defer db.Collection(name).Drop(ctx)
	}
	repo := NewRepository(db, 10)
	j, err := repo.Submit(ctx, Submission{Kind: "write", Key: "restart", Actor: "editor", Items: []Input{{Key: "one", Data: bson.M{"title": "frozen"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Claim(ctx, "write", "dead-process", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("workflow_jobs").UpdateOne(ctx, bson.M{"_id": j.ID}, bson.M{"$set": bson.M{"lease_until": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	// A fresh repository and runner have no in-memory submission information.
	fresh := NewRepository(db, 10)
	runner := NewRunner(fresh, 1)
	runner.Register("write", Executor{Authorize: func(context.Context, Job) error { return nil }, Run: func(ctx context.Context, job Job) error {
		rows, err := fresh.Items(ctx, job.ID, "", 10)
		if err != nil {
			return err
		}
		return fresh.CommitItem(ctx, job, rows[0].Key, func(sc mongo.SessionContext) (bson.M, error) {
			_, err := db.InsertOne(sc, "workflow_output", rows[0].Input)
			return bson.M{"saved": true}, err
		})
	}})
	if err := runner.RunOnce(ctx, "write"); err != nil {
		t.Fatal(err)
	}
	var output bson.M
	if err := db.Collection("workflow_output").FindOne(ctx, bson.M{}).Decode(&output); err != nil {
		t.Fatal(err)
	}
	if output["title"] != "frozen" {
		t.Fatalf("lost frozen input: %+v", output)
	}
}
