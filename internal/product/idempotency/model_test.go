package idempotency

// Pure unit tests: no Mongo required, always run in the fast gate.

import (
	"testing"
	"time"
)

func TestCanonicalHash(t *testing.T) {
	a := CanonicalHash([]byte(`{"title":"Hello","slug":"hello"}`))
	b := CanonicalHash([]byte(`{ "slug" : "hello" , "title" : "Hello" }`))
	if a != b {
		t.Fatalf("semantically identical bodies hash differently: %s vs %s", a, b)
	}
	c := CanonicalHash([]byte(`{"title":"Hello changed","slug":"hello"}`))
	if a == c {
		t.Fatalf("different bodies must hash differently")
	}
	if len(a) < 8 || a[:7] != "sha256:" {
		t.Fatalf("hash %q missing sha256: prefix", a)
	}
	empty1 := CanonicalHash(nil)
	empty2 := CanonicalHash([]byte("   "))
	if empty1 != empty2 {
		t.Fatalf("empty/whitespace bodies must hash identically")
	}
	raw1 := CanonicalHash([]byte("not json {{"))
	raw2 := CanonicalHash([]byte("not json {{"))
	if raw1 != raw2 {
		t.Fatalf("non-JSON bodies must hash deterministically")
	}
}

func TestOptionsValidation(t *testing.T) {
	// Invalid options fail before any DB access (nil DB is safe here).
	for _, opts := range []Options{
		{TTLHours: MaxTTLHours + 1, LeaseMinutes: 5},
		{TTLHours: -1, LeaseMinutes: 5},
		{TTLHours: 24, LeaseMinutes: -1},
	} {
		if _, err := NewService(nil, opts); CodeOf(err) != CodeInvalidRequest {
			t.Fatalf("opts %+v: got %v, want %s", opts, err, CodeInvalidRequest)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	if CodeOf(nil) != "" {
		t.Fatalf("CodeOf(nil) must be empty")
	}
	if CodeOf(&Error{Code: CodeConflict}) != CodeConflict {
		t.Fatalf("CodeOf must unwrap *Error")
	}
	if HeartbeatInterval != 60*time.Second {
		t.Fatalf("HeartbeatInterval = %v, want 60s (spec §21.5)", HeartbeatInterval)
	}
	if DefaultTTLHours != 24 || MinTTLHours != 1 || MaxTTLHours != 72 {
		t.Fatalf("TTL defaults out of spec §21.4 range: %d/%d/%d", DefaultTTLHours, MinTTLHours, MaxTTLHours)
	}
	if DefaultLeaseMinutes != 5 {
		t.Fatalf("DefaultLeaseMinutes = %d, want 5 (spec §21.5)", DefaultLeaseMinutes)
	}
}
