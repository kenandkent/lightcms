// R10 single-instance gate + R12 production base-URL gate tests.
//
// Run only these (per task constraints — never the full suite):
//
//	MONGODB_URI='mongodb://127.0.0.1:59351/lightcms-test?replicaSet=rs0&directConnection=true' \
//	  go test ./cmd/server/ -run 'TestInstanceGate|TestRequireProductionBaseURL' -count=1 -p 1
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	backdated := before.LastHeartbeat.Add(-time.Minute)
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

// deadPid spawns and reaps a sacrificial process, returning a pid that is
// deterministically dead (portable; avoids pid-range assumptions that vary
// by OS — e.g. darwin answers EINVAL instead of ESRCH for huge pids).
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sacrificial process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sacrificial process: %v", err)
	}
	_ = cmd.Wait()
	if pidAlive(pid) {
		t.Fatalf("sacrificial pid %d still reports alive", pid)
	}
	return pid
}

// TestInstanceGateReapsStaleLocalRival: a fresh-heartbeat rival record from
// THIS host whose pid is dead (previous incarnation, clean or crashed stop)
// must be reaped — restarts within the TTL window boot instead of flapping.
func TestInstanceGateReapsStaleLocalRival(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	staleRival := fmt.Sprintf("%s-%d-%d", localHostname(), deadPid(t), time.Now().Unix())
	if _, err := db.Collection(instanceLivenessCollection).InsertOne(ctx, bson.M{
		"_id": staleRival, "last_heartbeat": time.Now().UTC(),
		"started_at": time.Now().UTC(), "version": "v0",
	}); err != nil {
		t.Fatalf("seed stale rival: %v", err)
	}
	stop, err := enforceSingleInstance(ctx, db, gateTestID("test-restart"), "v1")
	if err != nil {
		t.Fatalf("restart must reap the stale local rival, got: %v", err)
	}
	defer stop()
	if n, _ := db.Collection(instanceLivenessCollection).CountDocuments(ctx,
		bson.M{"_id": staleRival}); n != 0 {
		t.Fatal("stale rival record must be reaped")
	}
}

// TestInstanceGateRejectsLiveLocalRival: same host but a LIVE pid is a
// genuine rival (possibly us in another process) — fail closed.
func TestInstanceGateRejectsLiveLocalRival(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	liveRival := fmt.Sprintf("%s-%d-%d", localHostname(), os.Getpid(), time.Now().Unix())
	if _, err := db.Collection(instanceLivenessCollection).InsertOne(ctx, bson.M{
		"_id": liveRival, "last_heartbeat": time.Now().UTC(),
		"started_at": time.Now().UTC(), "version": "v0",
	}); err != nil {
		t.Fatalf("seed live rival: %v", err)
	}
	if _, err := enforceSingleInstance(ctx, db, gateTestID("test-collide"), "v1"); err == nil {
		t.Fatal("live local rival must be rejected")
	} else if !strings.Contains(err.Error(), "multi-instance") {
		t.Fatalf("rejection must mention multi-instance, got: %v", err)
	}
}

// TestInstanceGateMalformedRivalRejects: unparsable rival IDs fail closed.
func TestInstanceGateMalformedRivalRejects(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	if _, err := db.Collection(instanceLivenessCollection).InsertOne(ctx, bson.M{
		"_id": "not-an-instance-id", "last_heartbeat": time.Now().UTC(),
		"started_at": time.Now().UTC(), "version": "v0",
	}); err != nil {
		t.Fatalf("seed malformed rival: %v", err)
	}
	if _, err := enforceSingleInstance(ctx, db, gateTestID("test-malformed"), "v1"); err == nil {
		t.Fatal("malformed rival must be rejected (fail closed)")
	}
	if host, pid, ok := parseInstanceID("not-an-instance-id"); ok || host != "" || pid != 0 {
		t.Fatalf("parseInstanceID must reject garbage, got %q %d %v", host, pid, ok)
	}
	if host, pid, ok := parseInstanceID(localHostname() + "-123-456"); !ok || pid != 123 {
		t.Fatalf("parseInstanceID must parse dashed hostnames, got %q %d %v", host, pid, ok)
	}
}

// TestInstanceGateStopRemovesRecord: graceful stop deletes our own record so
// the next boot never meets it.
func TestInstanceGateStopRemovesRecord(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	id := gateTestID("test-stop")
	stop, err := enforceSingleInstance(ctx, db, id, "v1")
	if err != nil {
		t.Fatalf("enter gate: %v", err)
	}
	stop()
	if n, _ := db.Collection(instanceLivenessCollection).CountDocuments(ctx,
		bson.M{"_id": id}); n != 0 {
		t.Fatal("stop() must remove our own liveness record")
	}
}

// TestInstanceGateStalePlusLiveRejects: one stale local rival plus one live
// foreign rival must still reject — reaping the stale one must never
// green-light booting alongside the live one (split-brain guard).
func TestInstanceGateStalePlusLiveRejects(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.Collection(instanceLivenessCollection).Drop(ctx)
	defer func() { _ = db.Collection(instanceLivenessCollection).Drop(ctx) }()

	stale := map[string]any{
		"_id":            fmt.Sprintf("%s-%d-%d", localHostname(), deadPid(t), time.Now().Unix()),
		"last_heartbeat": time.Now().UTC(), "started_at": time.Now().UTC(), "version": "v0",
	}
	live := map[string]any{
		"_id": "foreign-host-4242-1234567890", "last_heartbeat": time.Now().UTC(),
		"started_at": time.Now().UTC(), "version": "v0",
	}
	for _, doc := range []map[string]any{stale, live} {
		if _, err := db.Collection(instanceLivenessCollection).InsertOne(ctx, doc); err != nil {
			t.Fatalf("seed rival: %v", err)
		}
	}
	if _, err := enforceSingleInstance(ctx, db, gateTestID("test-mixed"), "v1"); err == nil {
		t.Fatal("live foreign rival alongside stale local must be rejected")
	} else if !strings.Contains(err.Error(), "multi-instance") {
		t.Fatalf("rejection must mention multi-instance, got: %v", err)
	}
	// The stale local was still reaped as a side effect.
	if n, _ := db.Collection(instanceLivenessCollection).CountDocuments(ctx,
		bson.M{"_id": stale["_id"]}); n != 0 {
		t.Fatal("stale local rival should have been reaped even on rejection")
	}
}
