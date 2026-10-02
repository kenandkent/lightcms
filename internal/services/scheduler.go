package services

import (
	"context"
	"log"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SchedulerService runs a background ticker that publishes content whose
// publish_at timestamp has passed.
type SchedulerService struct {
	db             *database.DB
	contentService *ContentService
	ticker         *time.Ticker
	done           chan struct{}
}

// NewSchedulerService creates a new SchedulerService.
func NewSchedulerService(db *database.DB, cs *ContentService) *SchedulerService {
	return &SchedulerService{
		db:             db,
		contentService: cs,
		done:           make(chan struct{}),
	}
}

// Start launches the background goroutine with a 60-second ticker.
func (s *SchedulerService) Start(ctx context.Context) {
	s.ticker = time.NewTicker(60 * time.Second)
	go func() {
		for {
			select {
			case <-s.done:
				return
			case <-ctx.Done():
				return
			case <-s.ticker.C:
				s.runOnce(ctx)
			}
		}
	}()
}

// Stop stops the ticker and signals the goroutine to exit.
func (s *SchedulerService) Stop() {
	close(s.done)
	if s.ticker != nil {
		s.ticker.Stop()
	}
}

// runOnce queries for due content and publishes each item.
// Task 16D: every item publishes through PublicationService under a stable
// operation key (scheduler/<id>/v<version>) — a tick retry replays instead
// of minting a duplicate Publication/outbox row.
func (s *SchedulerService) runOnce(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	filter := bson.M{
		"publish_at": bson.M{"$lte": time.Now()},
		"published":  false,
		"deleted":    bson.M{"$ne": true},
		// Lane 1B: never auto-publish sandbox or unapproved rows.
		// fork_id:nil matches both missing and null (live rows omit the
		// field via omitempty); pending_approval $ne:true covers
		// missing/false (approval sets explicit true).
		"fork_id":          nil,
		"pending_approval": bson.M{"$ne": true},
	}

	cursor, err := s.db.FindMany(runCtx, "content", filter,
		// The loop only reads _id + current_version: project so the ~4KB
		// embedding vector (and the full data map) never crosses the wire.
		options.Find().SetProjection(bson.M{"current_version": 1}))
	if err != nil {
		log.Printf("[scheduler] query failed: %v", err)
		return
	}
	defer cursor.Close(runCtx)

	type minContent struct {
		ID             primitive.ObjectID `bson:"_id"`
		CurrentVersion int64              `bson:"current_version"`
	}

	for cursor.Next(runCtx) {
		var item minContent
		if err := cursor.Decode(&item); err != nil {
			log.Printf("[scheduler] decode error: %v", err)
			continue
		}
		key := SchedulerOpKey(item.ID, item.CurrentVersion)
		if err := s.contentService.PublishInternal(runCtx, item.ID,
			"scheduler", "/internal/scheduler/publish", key); err != nil {
			log.Printf("[scheduler] failed to publish %s: %v", item.ID.Hex(), err)
		} else {
			log.Printf("[scheduler] published content %s", item.ID.Hex())
		}
	}
}
