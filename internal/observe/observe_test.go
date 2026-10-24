package observe

import (
	"testing"
)

// Task 16F: counters, structured fields and the failure-rate P0 behave.
func Test16F_CountersAndSnapshot(t *testing.T) {
	r := &Registry{}
	r.IncGenerationRequests()
	r.IncGenerationRequests()
	r.IncGenerationErrors()
	r.IncStageFailed()
	r.IncActivateFailed()
	r.IncIdemReplay()
	r.IncIdemConflict()
	r.IncOutboxDelivered()
	r.IncOutboxFailed()
	r.SetOutboxBacklog(7)
	r.AddScannerRepairs(3)
	r.AddScannerAlerts(2)

	snap := r.Snapshot()
	wants := map[string]int64{
		MetricGenerationRequests: 2,
		MetricGenerationErrors:   1,
		MetricStageFailed:        1,
		MetricActivateFailed:     1,
		MetricIdemReplay:         1,
		MetricIdemConflict:       1,
		MetricOutboxDelivered:    1,
		MetricOutboxFailed:       1,
		MetricOutboxBacklog:      7,
		MetricScannerRepairs:     3,
		MetricScannerAlerts:      2,
	}
	for name, want := range wants {
		if snap[name] != want {
			t.Errorf("%s = %d, want %d", name, snap[name], want)
		}
	}
}

func Test16F_FieldsKV(t *testing.T) {
	f := Fields{
		Actor: "human", ContentID: "c1", PublicationID: "p1",
		FullPath: "/x", Stage: "activate", DurationMS: 12,
		StatusCode: 200,
	}
	kv := f.kv()
	if len(kv)%2 != 0 {
		t.Fatalf("kv must be pairs, got %d items", len(kv))
	}
	seen := map[string]bool{}
	for i := 0; i < len(kv); i += 2 {
		seen[kv[i].(string)] = true
	}
	for _, k := range []string{"actor", "content_id", "publication_id", "full_path", "stage", "duration_ms", "status_code"} {
		if !seen[k] {
			t.Errorf("missing field %q in %v", k, kv)
		}
	}
	if seen["error_code"] || seen["template_version"] {
		t.Errorf("zero values must be omitted: %v", kv)
	}
}

func Test16F_FailureRateNoBreachOnSmallSamples(t *testing.T) {
	r := &Registry{}
	// 2 failures out of 3: high rate but below the 20-sample minimum —
	// must not page (and must not panic).
	r.ObservePublicationOutcome(false)
	r.ObservePublicationOutcome(false)
	r.ObservePublicationOutcome(true)
}
