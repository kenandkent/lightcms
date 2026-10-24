package storage

// R03 regression: republish cutover must never expose a missing canonical.
// The old sequence renamed canonical→previous FIRST, then spent arbitrarily
// long preparing .next (read + verify + write + verify) — concurrent readers
// observed ENOENT. The new sequence prepares .next fully, copies the backup,
// and flips with one atomic rename. This test stats the canonical in a tight
// loop during a multi-MiB republish and fails on a single miss.

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"
)

func TestCutoverNoServingGap(t *testing.T) {
	ctx := context.Background()
	s, _ := gapStore(t)

	oldBytes := bytes.Repeat([]byte("o"), 2<<20)
	v1 := gapStage(t, s, "/gap/nogap", oldBytes)
	if err := s.Activate(ctx, v1, "/gap/nogap"); err != nil {
		t.Fatalf("Activate v1: %v", err)
	}
	oldID := v1.PublicationID
	canon := mustCanonicalGap(t, s, "/gap/nogap")

	newBytes := bytes.Repeat([]byte("n"), 2<<20)
	v2 := gapStage(t, s, "/gap/nogap", newBytes)

	var missing int64
	var partial int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				b, err := os.ReadFile(canon)
				if err != nil {
					atomic.AddInt64(&missing, 1)
					continue
				}
				if len(b) != len(oldBytes) && len(b) != len(newBytes) {
					atomic.AddInt64(&partial, 1)
				}
			}
		}
	}()
	if err := s.ActivateWithPrevious(ctx, v2, "/gap/nogap", &oldID); err != nil {
		close(stop)
		<-done
		t.Fatalf("ActivateWithPrevious: %v", err)
	}
	close(stop)
	<-done

	if m := atomic.LoadInt64(&missing); m != 0 {
		t.Fatalf("canonical missing during cutover: %d observations (serving gap)", m)
	}
	if p := atomic.LoadInt64(&partial); p != 0 {
		t.Fatalf("canonical partial during cutover: %d observations", p)
	}
	if got, err := os.ReadFile(canon); err != nil || !bytes.Equal(got, newBytes) {
		t.Fatalf("canonical after cutover: err=%v len=%d", err, len(got))
	}
	// The recoverable backup holds the complete old bytes.
	prev, err := os.ReadFile(canon + ".previous-" + oldID.Hex())
	if err != nil || !bytes.Equal(prev, oldBytes) {
		t.Fatalf("previous backup: err=%v len=%d", err, len(prev))
	}
	// Confirmation removes the backup (post-commit cleanup, unchanged).
	if err := s.ConfirmActivate(ctx, "/gap/nogap", oldID); err != nil {
		t.Fatalf("ConfirmActivate: %v", err)
	}
	if _, err := os.Stat(canon + ".previous-" + oldID.Hex()); !os.IsNotExist(err) {
		t.Fatalf("previous not confirmed away: %v", err)
	}
}
