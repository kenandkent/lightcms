// R10 single-instance gate + R12 production base-URL gate tests.
//
// Run only these (per task constraints — never the full suite):
// MONGODB_URI='mongodb://127.0.0.1:59351/lightcms-test?replicaSet=rs0&directConnection=true' \
//   go test ./cmd/server/ -run 'TestInstanceGate|TestRequireProductionBaseURL' -count=1 -p 1
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
)

func gateTestID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestInstanceGateRejectsSecondLiveInstance: two different instance IDs enter
// the gate in turn — the second fails and the error mentions multi-instance.
func TestInstanceGateRejectsSecondLiveInstance(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	firstID := gateTestID("test-inst-a")
	secondID := gateTestID("test-inst-b")

	stopFirst, err := enforceSingleInstance(ctx, db, firstID, "v1")
	if err != nil {
		t.Fatalf("first instance must enter the gate: %v", err)
	}
	defer stopFirst()

	stopSecond, err := enforceSingleInstance(ctx, db, secondID, "v1")
	if err == nil {
		if stopSecond != nil {
			stopSecond()
		}
		t.Fatal("second live instance must be rejected")
	}
	if !strings.Contains(err.Error(), "multi-instance") {
		t.Fatalf("rejection must mention multi-instance, got: %v", err)
	}
	if !strings.Contains(err.Error(), firstID) {
		t.Fatalf("rejection must name the rival instance %q, got: %v", firstID, err)
	}
}

// TestInstanceGateAllowsAfterExpiry: a rival record whose heartbeat is 20
// minutes old (outside the 600s window) does not block entry.
func TestInstanceGateAllowsAfterExpiry(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	stale := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := db.Collection(instanceLivenessCollection).InsertOne(ctx, bson.M{
		"_id":            gateTestID("test-inst-stale"),
		"started_at":     stale,
		"last_heartbeat": stale,
		"version":        "v0",
	}); err != nil {
		t.Fatalf("seed stale rival record: %v", err)
	}

	stop, err := enforceSingleInstance(ctx, db, gateTestID("test-inst-fresh"), "v1")
	if err != nil {
		t.Fatalf("stale heartbeat must not block entry: %v", err)
	}
	defer stop()
}

// TestInstanceGateHeartbeatRefresh: after entering the gate, the record's
// last_heartbeat advances on the refresh cycle; stop() is idempotent.
func TestInstanceGateHeartbeatRefresh(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	oldInterval := instanceHeartbeatInterval
	instanceHeartbeatInterval = 50 * time.Millisecond
	defer func() { instanceHeartbeatInterval = oldInterval }()

	id := gateTestID("test-inst-hb")
	stop, err := enforceSingleInstance(ctx, db, id, "v1")
	if err != nil {
		t.Fatalf("enter gate: %v", err)
	}

	var before struct {
		LastHeartbeat time.Time `bson:"last_heartbeat"`
	}
	if err := db.Collection(instanceLivenessCollection).FindOne(ctx, bson.M{"_id": id}).Decode(&before); err != nil {
		stop()
		t.Fatalf("read initial heartbeat: %v", err)
	}

	// Direct refresh call must also bump the heartbeat (backdate first so
	// the bump is observable even without waiting for the ticker).
	backdated := before.LastHeartbeat.Add(-time.Hour)
	if _, err := db.Collection(instanceLivenessCollection).UpdateOne(ctx,
		bson.M{"_id": id}, bson.M{"$set": bson.M{"last_heartbeat": backdated}}); err != nil {
		stop()
		t.Fatalf("backdate heartbeat: %v", err)
	}
	if err := refreshInstanceHeartbeat(ctx, db, instanceLivenessCollection, id); err != nil {
		stop()
		t.Fatalf("direct refresh: %v", err)
	}
	var bumped struct {
		LastHeartbeat time.Time `bson:"last_heartbeat"`
	}
	if err := db.Collection(instanceLivenessCollection).FindOne(ctx, bson.M{"_id": id}).Decode(&bumped); err != nil {
		stop()
		t.Fatalf("read bumped heartbeat: %v", err)
	}
	if !bumped.LastHeartbeat.After(backdated) {
		stop()
		t.Fatalf("direct refresh must advance heartbeat (backdated=%v bumped=%v)", backdated, bumped.LastHeartbeat)
	}

	// Ticker-driven refresh must keep advancing it.
	deadline := time.Now().Add(10 * time.Second)
	var after struct {
		LastHeartbeat time.Time `bson:"last_heartbeat"`
	}
	for {
		if err := db.Collection(instanceLivenessCollection).FindOne(ctx, bson.M{"_id": id}).Decode(&after); err != nil {
			stop()
			t.Fatalf("read refreshed heartbeat: %v", err)
		}
		if after.LastHeartbeat.After(bumped.LastHeartbeat) {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("ticker never refreshed heartbeat (bumped=%v last=%v)", bumped.LastHeartbeat, after.LastHeartbeat)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// stop() must be idempotent (second call must not panic or block).
	stop()
	stop()
}

// TestRequireProductionBaseURL: production requires absolute HTTPS;
// anything else passes through (dev stays warn-only).
func TestRequireProductionBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		url     string
		wantErr bool
	}{
		{"production http fails", "production", "http://foo", true},
		{"production https passes", "production", "https://example.com", false},
		{"prod http fails", "prod", "http://foo", true},
		{"prod https passes", "prod", "https://example.com", false},
		{"production empty fails", "production", "", true},
		{"production query fails", "production", "https://example.com/?x=1", true},
		{"production https with path passes", "production", "https://example.com/blog", false},
		{"dev http passes", "development", "http://foo", false},
		{"dev empty passes", "dev", "", false},
		{"staging http passes", "staging", "http://foo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireProductionBaseURL(c.env, c.url)
			if c.wantErr && err == nil {
				t.Fatalf("requireProductionBaseURL(%q, %q) = nil, want error", c.env, c.url)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("requireProductionBaseURL(%q, %q) = %v, want nil", c.env, c.url, err)
			}
		})
	}
}
