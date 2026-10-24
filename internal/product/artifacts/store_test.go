package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

type failedObjectReader struct{ read bool }

func (r *failedObjectReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, syscall.ENOSPC
	}
	r.read = true
	return copy(p, "partial object"), nil
}

func TestObjectFailedWriteLeavesNoPublishedObjectOrTemporaryFile(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if ref, err := store.Put(context.Background(), &failedObjectReader{}, "text/plain", 100); !errors.Is(err, syscall.ENOSPC) || ref.Hash != "" {
		t.Fatalf("partial artifact returned: %+v %v", ref, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "objects", "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed temporary write leaked: %d %v", len(entries), err)
	}
	// A destination filesystem failure must also fail closed, without leaving
	// a readable half-object. Do not fill the developer's actual disk.
	// The actual expected hash shard is made unwritable by a file collision.
	probe, err := store.Put(context.Background(), bytes.NewBufferString("shard probe"), "text/plain", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "objects", probe.Hash[:2], probe.Hash)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "objects", probe.Hash[:2])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "objects", probe.Hash[:2]), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if ref, err := store.Put(context.Background(), bytes.NewBufferString("shard probe"), "text/plain", 100); err == nil || ref.Hash != "" {
		t.Fatalf("filesystem failure returned object: %+v %v", ref, err)
	}
}

func TestAdmissionConcurrentLoadStaysWithinBudget(t *testing.T) {
	const budget = 32 << 20
	gate := NewAdmission(budget)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for n := 0; n < 64; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := gate.Acquire(ctx, 4<<20)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			buffer := make([]byte, 4<<20)
			for k := 0; k < len(buffer); k += 4096 {
				buffer[k] = 1
			}
			time.Sleep(time.Millisecond)
		}()
	}
	wg.Wait()
	if gate.Stats().Used != 0 || gate.Peak() > budget {
		t.Fatalf("capacity leak or overrun: used=%d peak=%d", gate.Stats().Used, gate.Peak())
	}
	t.Logf("64 concurrent requests, 4MiB each, 32MiB admission budget; peak=%d bytes", gate.Peak())
}

func TestObjectsDeduplicateAndRejectCorruption(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.Put(ctx, bytes.NewBufferString("immutable"), "text/plain", 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(ctx, bytes.NewBufferString("immutable"), "text/plain", 100)
	if err != nil || first.Hash != second.Hash {
		t.Fatalf("dedup failed: %v", err)
	}
	reader, err := store.Open(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(body) != "immutable" {
		t.Fatalf("wrong object: %q %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(root, "objects", first.Hash[:2], first.Hash), []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, first); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt object trusted: %v", err)
	}
}
func TestObjectLimitsAndPaths(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.Put(ctx, bytes.NewBufferString("12345"), "text/plain", 4); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversized object accepted: %v", err)
	}
	if _, err := store.Open(ctx, Ref{Hash: "../../outside"}); err == nil {
		t.Fatal("invalid object path accepted")
	}
}
func TestAdmissionBoundsAggregateReservations(t *testing.T) {
	gate := NewAdmission(10)
	release, err := gate.Acquire(context.Background(), 6)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.Acquire(ctx, 6); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("aggregate budget ignored: %v", err)
	}
	release()
	release() // release is idempotent, cannot create extra capacity
	next, err := gate.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	next()
	if _, err := gate.Acquire(context.Background(), 11); !errors.Is(err, ErrLimit) {
		t.Fatalf("impossible reservation accepted: %v", err)
	}
	if gate.Peak() > 10 {
		t.Fatal("capacity exceeded")
	}
}

func TestObjectLifecycleLockHonorsCancellation(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ref, err := store.Put(context.Background(), bytes.NewBufferString("locked"), "text/plain", 100)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.lockObject(context.Background(), ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if second, err := store.lockObject(ctx, ref.Hash); !errors.Is(err, context.DeadlineExceeded) {
		if second != nil {
			second()
		}
		t.Fatalf("concurrent lifecycle lock accepted: %v", err)
	}
}
