// R10 single-instance startup gate + R12 production base-URL gate.
//
// R10: only one server process may serve a site database at a time. Boot
// registers this instance in the instance_liveness collection (TTL-guarded)
// and fails fast when another instance holds a fresh heartbeat, so a
// multi-instance deployment can never silently split-brain content writes.
// A background goroutine refreshes our heartbeat until stop() is called.
//
// R12: production must serve from a canonical HTTPS public origin; the
// check is a pure function (requireProductionBaseURL) shared with main.go.
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// instanceLivenessCollection holds one document per running server instance.
const instanceLivenessCollection = "instance_liveness"
const writerLeaseSlot = "__exclusive_site_writer__"

// instanceHeartbeatInterval is how often the gate refreshes our own
// heartbeat. instanceLivenessWindow is how far back a rival heartbeat counts
// as "live" (and the TTL the index enforces server-side). Vars (not consts)
// so tests can shrink the refresh interval.
var (
	instanceHeartbeatInterval        = 60 * time.Second
	instanceLivenessWindow           = 600 * time.Second
	instanceLivenessTTLSeconds int32 = 600
	instanceLivenessIndexName        = "instance_liveness_heartbeat_ttl"
)

// newInstanceID builds a unique-per-boot instance identity from
// hostname + pid + start time (unix seconds).
func newInstanceID() string {
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().Unix())
}

// isInstanceIndexExistsError reports already-exists-class index build errors
// (create race against a parallel boot, or a conflicting legacy shape).
func isInstanceIndexExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "indexoptionsconflict") ||
		strings.Contains(msg, "indexkeyspecsconflict")
}

// ensureInstanceLivenessIndex creates the TTL index on last_heartbeat
// (expireAfterSeconds=600) when absent; already-exists-class races are
// tolerated, genuine failures are returned.
func ensureInstanceLivenessIndex(ctx context.Context, db *database.DB, coll string) error {
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "last_heartbeat", Value: 1}},
		Options: options.Index().SetName(instanceLivenessIndexName).SetExpireAfterSeconds(instanceLivenessTTLSeconds),
	}
	if _, err := db.Collection(coll).Indexes().CreateOne(ctx, model); err != nil {
		if isInstanceIndexExistsError(err) {
			return nil
		}
		return err
	}
	return nil
}

// refreshInstanceHeartbeat bumps our own liveness heartbeat to now.
func refreshInstanceHeartbeat(ctx context.Context, db *database.DB, coll, instanceID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := db.Collection(coll).UpdateOne(rctx,
		bson.M{"_id": instanceID, "last_heartbeat": bson.M{"$gt": time.Now().UTC().Add(-instanceLivenessWindow)}},
		bson.M{"$set": bson.M{"last_heartbeat": time.Now().UTC()}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount != 1 {
		return fmt.Errorf("single-instance ownership lost: missing or expired heartbeat")
	}
	return nil
}

// enforceSingleInstance registers instanceID in the liveness collection and
// fails when another instance holds a heartbeat within the liveness window.
// On success it starts a background goroutine refreshing our heartbeat
// every instanceHeartbeatInterval; the returned stop func halts it (stop is
// idempotent) and best-effort deletes our own record so clean restarts never
// meet their own pre-stop heartbeat (guarded by started_at; crashes still
// rely on TTL expiry plus stale-local reaping).
func enforceSingleInstance(ctx context.Context, db *database.DB, instanceID string, version string) (func(), error) {
	return enforceSingleInstanceOnCollection(ctx, db, instanceLivenessCollection, instanceID, version, func(err error) { log.Fatalf("single-instance writer lease lost; stopping process: %v", err) })
}

// enforceSingleInstanceOnCollection is the collection-parameterized core so
// tests can isolate without touching the production collection name.
func enforceSingleInstanceOnCollection(ctx context.Context, db *database.DB, coll, instanceID, version string, onLost ...func(error)) (func(), error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, fmt.Errorf("single-instance gate: instance ID is required")
	}
	if err := ensureInstanceLivenessIndex(ctx, db, coll); err != nil {
		return nil, fmt.Errorf("single-instance gate: ensure liveness index: %w", err)
	}
	now := time.Now().UTC()
	if _, err := db.Collection(coll).UpdateOne(ctx,
		bson.M{"_id": instanceID},
		bson.M{
			"$set":         bson.M{"last_heartbeat": now, "version": version},
			"$setOnInsert": bson.M{"started_at": now},
		},
		options.Update().SetUpsert(true),
	); err != nil {
		return nil, fmt.Errorf("single-instance gate: register instance: %w", err)
	}
	// Every rival with a heartbeat inside the liveness window blocks boot —
	// EXCEPT stale incarnations on this host whose processes are gone
	// (clean restarts must not trip over their own pre-stop heartbeat;
	// crash recovery must not wait out the TTL). ALL fresh rivals are
	// examined: reaping one stale local record must never green-light
	// booting alongside a live one (split-brain).
	var rivalIDs []string
	cursor, cerr := db.Collection(coll).Find(ctx,
		bson.M{
			"_id":            bson.M{"$nin": bson.A{instanceID, writerLeaseSlot}},
			"last_heartbeat": bson.M{"$gte": now.Add(-instanceLivenessWindow)},
		},
		options.Find().SetProjection(bson.M{"_id": 1}))
	if cerr != nil {
		_, _ = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": instanceID})
		return nil, fmt.Errorf("single-instance gate: list rival instances: %w", cerr)
	}
	for cursor.Next(ctx) {
		var r struct {
			ID string `bson:"_id"`
		}
		if derr := cursor.Decode(&r); derr != nil || r.ID == "" {
			// Undecodable/foreign record: fail closed, count it live.
			rivalIDs = append(rivalIDs, "<undecodable-liveness-record>")
			continue
		}
		rivalIDs = append(rivalIDs, r.ID)
	}
	cursor.Close(ctx)
	if cerr := cursor.Err(); cerr != nil {
		_, _ = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": instanceID})
		return nil, fmt.Errorf("single-instance gate: list rival instances: %w", cerr)
	}
	var liveRivals []string
	for _, rid := range rivalIDs {
		if host, pid, ok := parseInstanceID(rid); ok && host == localHostname() && !pidAlive(pid) {
			// Stale local incarnation: reap it and continue scanning.
			// Best effort: a failed reap fails closed below.
			if _, derr := db.Collection(coll).DeleteOne(ctx, bson.M{"_id": rid}); derr != nil {
				liveRivals = append(liveRivals, rid+" (reap failed)")
				continue
			}
			log.Printf("WARNING: single-instance gate reaped stale local incarnation %q (process %d gone); continuing scan", rid, pid)
			continue
		}
		liveRivals = append(liveRivals, rid)
	}
	if len(liveRivals) > 0 {
		// Remove our own probe so a rejected starter leaves no fresh
		// heartbeat behind (otherwise the next restart would trip over
		// our own litter and flap until the window passes).
		_, _ = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": instanceID})
		return nil, fmt.Errorf("instance %q is already running: multi-instance deployment is unsupported: run a single instance", liveRivals[0])
	}
	// A single atomic slot is the final arbiter, including simultaneous boot.
	// TTL/probe rows alone cannot establish an exclusive writer lease.
	var slot struct {
		Owner         string    `bson:"owner"`
		LastHeartbeat time.Time `bson:"last_heartbeat"`
	}
	if err := db.Collection(coll).FindOne(ctx, bson.M{"_id": writerLeaseSlot}).Decode(&slot); err == nil {
		if host, pid, ok := parseInstanceID(slot.Owner); ok && host == localHostname() && !pidAlive(pid) {
			_, _ = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": writerLeaseSlot, "owner": slot.Owner, "last_heartbeat": slot.LastHeartbeat})
		}
	}
	_, err := db.Collection(coll).UpdateOne(ctx, bson.M{"_id": writerLeaseSlot, "$or": bson.A{bson.M{"owner": instanceID}, bson.M{"last_heartbeat": bson.M{"$lte": now.Add(-instanceLivenessWindow)}}}}, bson.M{"$set": bson.M{"owner": instanceID, "started_at": now, "last_heartbeat": now}}, options.Update().SetUpsert(true))
	if err != nil {
		_, _ = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": instanceID})
		return nil, fmt.Errorf("multi-instance deployment unsupported: exclusive writer lease held by %q: %w", slot.Owner, err)
	}

	interval := instanceHeartbeatInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	stopCh := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(stopCh) })
		// Best-effort self-removal so clean restarts never meet their own
		// pre-stop heartbeat (crashes still rely on TTL + stale reaping).
		// Guarded by started_at: never delete a newer incarnation that
		// reused this ID. Uses a fresh context: the caller may already
		// be shutting down.
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Collection(coll).DeleteOne(dctx, bson.M{
			"_id": instanceID, "started_at": bson.M{"$lte": now},
		})
		_, _ = db.Collection(coll).DeleteOne(dctx, bson.M{"_id": writerLeaseSlot, "owner": instanceID, "started_at": now})
	}
	var lostOnce sync.Once
	lost := func(err error) {
		lostOnce.Do(func() {
			for _, fn := range onLost {
				if fn != nil {
					fn(err)
				}
			}
			stop()
		})
	}
	renewed := make(chan time.Time, 1)
	// Independent watchdog: a blocked Mongo heartbeat must not postpone
	// fail-stop past lease expiry. Stop slightly before the slot becomes claimable.
	guardWindow := instanceLivenessWindow - time.Second
	if guardWindow <= 0 {
		guardWindow = instanceLivenessWindow * 9 / 10
	}
	go func() {
		deadline := time.NewTimer(time.Until(now.Add(guardWindow)))
		defer deadline.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-deadline.C:
				lost(fmt.Errorf("writer lease renewal deadline exceeded"))
				return
			case at := <-renewed:
				if !deadline.Stop() {
					select {
					case <-deadline.C:
					default:
					}
				}
				deadline.Reset(time.Until(at.Add(guardWindow)))
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				// Heartbeats use a fresh background context: the boot
				// context may be a short connect-scoped timeout.
				if err := refreshInstanceHeartbeat(context.Background(), db, coll, instanceID); err != nil {
					lost(err)
					return
				}
				rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				renewedAt := time.Now().UTC()
				res, err := db.Collection(coll).UpdateOne(rctx, bson.M{"_id": writerLeaseSlot, "owner": instanceID, "started_at": now, "last_heartbeat": bson.M{"$gt": renewedAt.Add(-instanceLivenessWindow)}}, bson.M{"$set": bson.M{"last_heartbeat": renewedAt}})
				cancel()
				if err != nil {
					lost(err)
					return
				}
				if res.MatchedCount != 1 {
					lost(fmt.Errorf("exclusive writer slot ownership lost"))
					return
				}
				select {
				case renewed <- renewedAt:
				case <-stopCh:
					return
				}
			}
		}
	}()
	return stop, nil
}

// localHostname returns the OS hostname or "unknown" (same fallback as
// newInstanceID so the comparison matches what boot wrote).
func localHostname() string {
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return host
}

// parseInstanceID splits a newInstanceID-shaped identity ("host-pid-ts")
// from the right (hostnames may contain dashes). ok=false for foreign
// shapes — those always fail closed.
func parseInstanceID(id string) (host string, pid int, ok bool) {
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return "", 0, false
	}
	ts := id[i+1:]
	j := strings.LastIndex(id[:i], "-")
	if j < 0 {
		return "", 0, false
	}
	var pid64 int64
	var err error
	host, pidStr := id[:j], id[j+1:i]
	if host == "" || pidStr == "" || ts == "" {
		return "", 0, false
	}
	for _, c := range []byte(pidStr + ts) {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	var n int
	if n, err = fmt.Sscanf(pidStr, "%d", &pid64); n != 1 || err != nil || pid64 <= 0 {
		return "", 0, false
	}
	pid = int(pid64)
	_ = ts
	return host, pid, true
}

// requireProductionBaseURL enforces the R12 production startup rule:
// production envs ("production" and "prod", mirroring config.IsProd)
// require publicBaseURL to parse and pass
// publicurl.ValidateProductionBaseURL (absolute HTTPS, no userinfo/query/
// fragment). Any other env returns nil (dev keeps warn-only degradation).
func requireProductionBaseURL(env, publicBaseURL string) error {
	if env != "production" && env != "prod" {
		return nil
	}
	parsed, err := url.Parse(publicBaseURL)
	if err != nil {
		return fmt.Errorf("production PUBLIC_BASE_URL %q is not a valid URL: %w", publicBaseURL, err)
	}
	if err := publicurl.ValidateProductionBaseURL(parsed); err != nil {
		return fmt.Errorf("production PUBLIC_BASE_URL %q invalid: %w", publicBaseURL, err)
	}
	return nil
}
