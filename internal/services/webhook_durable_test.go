package services

import (
	"context"
	"errors"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOrdinaryWebhookSurvivesRestartAndRollsBack(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	var received atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-LightCMS-Event-ID") == "" {
			t.Error("missing stable event ID")
		}
		received.Add(1)
		w.WriteHeader(204)
	}))
	defer endpoint.Close()
	if _, err := db.InsertOne(ctx, "webhooks", WebhookDoc{URL: endpoint.URL, Events: []string{"content.create"}, Active: true}); err != nil {
		t.Fatal(err)
	}
	repo := jobs.NewRepository(db, 10)
	runner := jobs.NewRunner(repo, 1)
	s := NewWebhookService(db)
	s.EnableDurable(runner)
	if err := s.RecordEvent(ctx, "content.create", "committed", bson.M{"title": "Frozen"}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if err := s.RecordEvent(sc, "content.create", "aborted", bson.M{"title": "Must not deliver"}); err != nil {
			return err
		}
		return errors.New("rollback")
	}); err == nil {
		t.Fatal("expected rollback")
	}
	if received.Load() != 0 {
		t.Fatal("network delivery occurred before durable execution")
	}
	fresh := NewWebhookService(db)
	freshRunner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	fresh.EnableDurable(freshRunner)
	if err := freshRunner.RunOnce(ctx, "webhook"); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatalf("restart delivered %d events", received.Load())
	}
	if err := freshRunner.RunOnce(ctx, "webhook"); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatal("committed event delivered repeatedly or aborted event escaped")
	}
}
