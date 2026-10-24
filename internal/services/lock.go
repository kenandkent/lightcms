package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/database"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// LockDuration is how long a content lock remains valid before it expires.
const LockDuration = 30 * time.Minute

// ContentLockDoc represents an advisory lock on a content item.
type ContentLockDoc struct {
	Token      string             `bson:"token,omitempty" json:"token,omitempty"`
	ID         primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	ContentID  primitive.ObjectID `bson:"content_id" json:"content_id"`
	UserID     primitive.ObjectID `bson:"user_id" json:"user_id"`
	UserEmail  string             `bson:"user_email" json:"user_email"`
	AcquiredAt time.Time          `bson:"acquired_at" json:"acquired_at"`
	ExpiresAt  time.Time          `bson:"expires_at" json:"expires_at"`
}

// LockService manages advisory content locks.
type LockService struct {
	db         *database.DB
	indexMu    sync.Mutex
	indexReady bool
}

// NewLockService creates a new LockService.
func NewLockService(db *database.DB) *LockService {
	return &LockService{db: db}
}

// AcquireLock attempts to acquire a lock on contentID for the given user.
//
// Returns (nil, nil) if the lock was successfully acquired.
// Returns (existingLock, nil) if another user holds a non-expired lock.
// Returns (nil, err) on database errors.
func (s *LockService) AcquireLock(ctx context.Context, contentID, userID primitive.ObjectID, email string) (*ContentLockDoc, error) {
	lock, acquired, err := s.AcquireLease(ctx, contentID, userID, email)
	if acquired {
		return nil, err
	}
	return lock, err
}

// AcquireLease returns a generation token for clients that need protection
// against delayed releases from another tab belonging to the same user.
func (s *LockService) AcquireLease(ctx context.Context, contentID, userID primitive.ObjectID, email string) (*ContentLockDoc, bool, error) {
	// This index is the arbitration boundary. Never silently proceed if a
	// legacy database could not create it (for example, duplicate old rows).
	s.indexMu.Lock()
	if !s.indexReady {
		_, err := s.db.Collection("content_locks").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "content_id", Value: 1}}, Options: options.Index().SetUnique(true)})
		if err != nil {
			s.indexMu.Unlock()
			return nil, false, err
		}
		s.indexReady = true
	}
	s.indexMu.Unlock()
	for attempt := 0; attempt < 4; attempt++ {
		now := time.Now()
		doc := ContentLockDoc{ContentID: contentID, UserID: userID, UserEmail: email, AcquiredAt: now, ExpiresAt: now.Add(LockDuration), Token: primitive.NewObjectID().Hex()}
		matched, err := s.db.UpdateOneMatched(ctx, "content_locks", bson.M{
			"content_id": contentID,
			"$or":        bson.A{bson.M{"expires_at": bson.M{"$lte": now}}, bson.M{"user_id": userID}},
		}, bson.M{"$set": bson.M{"user_id": userID, "user_email": email, "acquired_at": now, "expires_at": doc.ExpiresAt, "token": doc.Token}})
		if err != nil {
			return nil, false, err
		}
		if matched == 1 {
			return &doc, true, nil
		}
		id, err := s.db.InsertOne(ctx, "content_locks", doc)
		if err == nil {
			doc.ID = id
			return &doc, true, nil
		}
		if !mongo.IsDuplicateKeyError(err) {
			return nil, false, err
		}
		blocking, err := s.GetLock(ctx, contentID)
		if err != nil {
			return nil, false, err
		}
		if blocking != nil && blocking.UserID != userID {
			return blocking, false, nil
		}
	}
	return nil, false, fmt.Errorf("editing lease changed concurrently; retry acquisition")
}

// ReleaseLock removes the lock for contentID held by userID.
func (s *LockService) ReleaseLock(ctx context.Context, contentID, userID primitive.ObjectID) error {
	return s.db.DeleteOne(ctx, "content_locks", bson.M{
		"content_id": contentID,
		"user_id":    userID,
	})
}

// GetLock returns the active (non-expired) lock for contentID, or nil if none.
func (s *LockService) GetLock(ctx context.Context, contentID primitive.ObjectID) (*ContentLockDoc, error) {
	var doc ContentLockDoc
	err := s.db.FindOne(ctx, "content_locks", bson.M{
		"content_id": contentID,
		"expires_at": bson.M{"$gt": time.Now()},
	}, &doc)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// RefreshLock extends the expiry of an existing lock by LockDuration for the same user.
func (s *LockService) RefreshLock(ctx context.Context, contentID, userID primitive.ObjectID) error {
	return s.refreshLease(ctx, bson.M{"content_id": contentID, "user_id": userID})
}

func (s *LockService) RefreshLease(ctx context.Context, contentID, userID primitive.ObjectID, token string) error {
	if token == "" {
		return fmt.Errorf("lease token is required")
	}
	return s.refreshLease(ctx, bson.M{"content_id": contentID, "user_id": userID, "token": token})
}

func (s *LockService) ReleaseLease(ctx context.Context, contentID, userID primitive.ObjectID, token string) error {
	if token == "" {
		return fmt.Errorf("lease token is required")
	}
	return s.db.DeleteOne(ctx, "content_locks", bson.M{"content_id": contentID, "user_id": userID, "token": token})
}

func (s *LockService) refreshLease(ctx context.Context, filter bson.M) error {
	filter["expires_at"] = bson.M{"$gt": time.Now()}
	matched, err := s.db.UpdateOneMatched(ctx, "content_locks", filter,
		bson.M{"$set": bson.M{"expires_at": time.Now().Add(LockDuration)}},
	)
	if err != nil {
		return err
	}
	if matched == 0 {
		return fmt.Errorf("editing lease expired or ownership changed")
	}
	return nil
}

// ForceUnlock removes any lock for contentID regardless of owner.
// The caller is responsible for verifying admin permissions.
func (s *LockService) ForceUnlock(ctx context.Context, contentID primitive.ObjectID) error {
	return s.db.DeleteOne(ctx, "content_locks", bson.M{"content_id": contentID})
}
