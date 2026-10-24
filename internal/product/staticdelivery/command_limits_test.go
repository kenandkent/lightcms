package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"testing"
	"time"
)

func TestRunnerExportTimeoutBlocksInsteadOfLooping(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	job := Job{Record: newRecord(), Kind: "export", State: JobQueued}
	if _, e := repo.collection("jobs").InsertOne(ctx, job); e != nil {
		t.Fatal(e)
	}
	r := NewRunner(repo, RunnerDependencies{Owner: "timeout-test", Timeout: 30 * time.Millisecond, Concurrency: 3, Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) {
		t.Fatal("export called Cloudflare")
		return nil, nil
	}, Export: func(ctx context.Context, _ Job) error { <-ctx.Done(); return ctx.Err() }})
	if e := r.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	var got Job
	if e := repo.collection("jobs").FindOne(ctx, bson.M{"_id": job.ID}).Decode(&got); e != nil {
		t.Fatal(e)
	}
	if got.State != JobFailed || got.ErrorCode != CodeJobTimeout {
		t.Fatalf("timeout not held for operator: %+v", got)
	}
	if r.deps.Concurrency != 3 {
		t.Fatal("configured concurrency ignored")
	}
}
