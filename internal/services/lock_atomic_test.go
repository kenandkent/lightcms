package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestLockAtomicExpiredRefresh(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	s := NewLockService(db)
	cid, uid := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "content_locks", ContentLockDoc{ContentID: cid, UserID: uid, ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshLock(ctx, cid, uid); err == nil {
		t.Fatal("expired lease must not be resurrected by a stale editor")
	}
	var lock ContentLockDoc
	if err := db.FindOne(ctx, "content_locks", bson.M{"content_id": cid}, &lock); err == nil && lock.ExpiresAt.After(time.Now()) {
		t.Fatal("expired lease was revived")
	}
}

func TestLockAtomicContenders(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	s := NewLockService(db)
	ctx := context.Background()
	cid := primitive.NewObjectID()
	start := make(chan struct{})
	results := make(chan bool, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			blocking, err := s.AcquireLock(ctx, cid, primitive.NewObjectID(), "editor")
			results <- err == nil && blocking == nil
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for won := range results {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected one lock owner, got %d", winners)
	}
}

func TestLockAtomicStaleTokenCannotRelease(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	s := NewLockService(db)
	cid, uid := primitive.NewObjectID(), primitive.NewObjectID()
	first, ok, err := s.AcquireLease(ctx, cid, uid, "editor")
	if err != nil || !ok {
		t.Fatalf("first: %v", err)
	}
	second, ok, err := s.AcquireLease(ctx, cid, uid, "editor")
	if err != nil || !ok {
		t.Fatalf("second: %v", err)
	}
	if err := s.ReleaseLease(ctx, cid, uid, first.Token); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLock(ctx, cid)
	if err != nil || got == nil || got.Token != second.Token {
		t.Fatalf("new lease removed: %+v %v", got, err)
	}
	if err := s.RefreshLease(ctx, cid, uid, first.Token); err == nil {
		t.Fatal("stale token refreshed lease")
	}
}
