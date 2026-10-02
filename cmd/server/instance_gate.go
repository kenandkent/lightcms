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

// instanceHeartbeatInterval is how often the gate refreshes our own
// heartbeat. instanceLivenessWindow is how far back a rival heartbeat counts
// as "live" (and the TTL the index enforces server-side). Vars (not consts)
// so tests can shrink the refresh interval.
var (
	instanceHeartbeatInterval     = 60 * time.Second
	instanceLivenessWindow        = 600 * time.Second
	instanceLivenessTTLSeconds    int32 = 600
	instanceLivenessIndexName           = "instance_liveness_heartbeat_ttl"
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
	_, err := db.Collection(coll).UpdateOne(rctx,
		bson.M{"_id": instanceID},
		bson.M{"$set": bson.M{"last_heartbeat": time.Now().UTC()}},
	)
	return err
}

// enforceSingleInstance registers instanceID in the liveness collection and
// fails when another instance holds a heartbeat within the liveness window.
// On success it starts a background goroutine refreshing our heartbeat
// every instanceHeartbeatInterval; the returned stop func halts it (stop is
// idempotent and only stops the refresher — the record is left for TTL
// expiry so a crash never looks like a clean leave).
func enforceSingleInstance(ctx context.Context, db *database.DB, instanceID string, version string) (func(), error) {
	return enforceSingleInstanceOnCollection(ctx, db, instanceLivenessCollection, instanceID, version)
}

// enforceSingleInstanceOnCollection is the collection-parameterized core so
// tests can isolate without touching the production collection name.
func enforceSingleInstanceOnCollection(ctx context.Context, db *database.DB, coll, instanceID, version string) (func(), error) {
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
	// Any rival with a heartbeat inside the liveness window blocks boot.
	var rival struct {
		ID string `bson:"_id"`
	}
	rivalErr := db.Collection(coll).FindOne(ctx,
		bson.M{
			"_id":            bson.M{"$ne": instanceID},
			"last_heartbeat": bson.M{"$gte": now.Add(-instanceLivenessWindow)},
		},
		options.FindOne().SetProjection(bson.M{"_id": 1}),
	).Decode(&rival)
	switch {
	case rivalErr == nil:
		// Remove our own probe so a rejected starter leaves no fresh
		// heartbeat behind (otherwise the next restart would trip over
		// our own litter and flap until the window passes).
		_, _ = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": instanceID})
		return nil, fmt.Errorf("instance %q is already running: multi-instance deployment is unsupported: run a single instance", rival.ID)
	case rivalErr == mongo.ErrNoDocuments:
		// Sole live instance — proceed.
	default:
		return nil, fmt.Errorf("single-instance gate: check rival instances: %w", rivalErr)
	}

	interval := instanceHeartbeatInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	stopCh := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(stopCh) })
	}
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
					log.Printf("WARNING: instance heartbeat refresh failed: %v", err)
				}
			}
		}
	}()
	return stop, nil
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
