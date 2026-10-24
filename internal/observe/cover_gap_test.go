package observe

// Task 17B coverage-gap tests: Default/Reset, log emitters, AlertP0,
// full-field kv, and the P0 breach path of ObservePublicationOutcome.

import (
	"testing"
)

func TestCoverGapDefaultResetAndEmitters(t *testing.T) {
	d := Default()
	if d == nil {
		t.Fatalf("Default() = nil")
	}
	// Exercise every counter + gauge on an isolated registry, then Reset.
	r := &Registry{}
	r.IncGenerationRequests()
	r.IncGenerationErrors()
	r.IncStageFailed()
	r.IncActivateFailed()
	r.IncIdemReplay()
	r.IncIdemConflict()
	r.IncOutboxDelivered()
	r.IncOutboxFailed()
	r.SetOutboxBacklog(9)
	r.AddScannerRepairs(4)
	r.AddScannerAlerts(5)
	snap := r.Snapshot()
	if snap[MetricOutboxBacklog] != 9 || snap[MetricScannerAlerts] != 5 {
		t.Fatalf("snapshot before reset: %v", snap)
	}
	r.Reset()
	for k, v := range r.Snapshot() {
		if v != 0 {
			t.Fatalf("after Reset: %s = %d, want 0", k, v)
		}
	}

	// Full-field kv covers every add() branch.
	full := Fields{
		RequestID: "req-1", Actor: "human", UserID: "u1", AgentSession: "sess",
		TemplateSlug: "news", TemplateVersion: 3, ContentID: "c1",
		ContentVersion: 7, PublicationID: "p1", FullPath: "/news/1",
		StorageProvider: "fs", Stage: "activate", DurationMS: 11,
		StatusCode: 200, ErrorCode: "E_X",
	}
	kv := full.kv()
	if len(kv) == 0 || len(kv)%2 != 0 {
		t.Fatalf("full kv: %d items", len(kv))
	}
	// Zero fields still emit duration_ms unconditionally.
	empty := Fields{}.kv()
	if len(empty) != 2 || empty[0] != "duration_ms" {
		t.Fatalf("empty kv = %v, want [duration_ms 0]", empty)
	}

	// Emitters must not panic (output goes to the process log).
	LogPublication("received", full)
	LogPublication("failed", Fields{})
	LogGeneration("ok", full)
	AlertP0("publication_failure_rate", "test alert", full)
	AlertP0("x", "empty fields", Fields{})
}

func TestCoverGapOutcomeBreach(t *testing.T) {
	r := &Registry{}
	// Below-threshold volume: no alert, just bucket rotation.
	for i := 0; i < 10; i++ {
		r.ObservePublicationOutcome(true)
	}
	// Breach: >=20 attempts in-window with >5% failures.
	for i := 0; i < 18; i++ {
		r.ObservePublicationOutcome(true)
	}
	r.ObservePublicationOutcome(false)
	r.ObservePublicationOutcome(false)
	snap := r.Snapshot()
	if len(snap) != 11 {
		t.Fatalf("snapshot keys = %d, want 11", len(snap))
	}
}
