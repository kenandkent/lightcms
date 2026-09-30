// Package observe owns the Task 16F publication observability surface
// (spec §37): structured request/publication logs, lifecycle counters and
// P0 alerts. It sits below all product packages (it imports only jsonlog)
// so the saga, idempotency service, outbox worker, scanner and HTTP
// handlers can all report into one registry without import cycles.
//
// Counters follow the spec §37.2 metric names. P0 alerts go to the existing
// operational surface: the process log with a [P0-ALERT] prefix (operators
// already grep it) plus a structured jsonlog error line.
package observe

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/jsonlog"
)

// Counter names (spec §37.2).
const (
	MetricGenerationRequests = "page_generation_requests_total"
	MetricGenerationErrors   = "page_generation_errors_total"
	MetricStageFailed        = "publication_stage_failed_total"
	MetricActivateFailed     = "publication_activate_failed_total"
	MetricIdemReplay         = "idempotency_replay_total"
	MetricIdemConflict       = "idempotency_conflict_total"
	MetricOutboxDelivered    = "outbox_delivered_total"
	MetricOutboxFailed       = "outbox_failed_total"
	MetricOutboxBacklog      = "outbox_backlog"
	MetricScannerRepairs     = "scanner_repairs_total"
	MetricScannerAlerts      = "scanner_alerts_total"
)

var global = &Registry{}

// Registry holds monotonically increasing lifecycle counters plus the
// outbox backlog gauge. Use Default (the process registry) unless a test
// needs isolation.
type Registry struct {
	generationRequests atomic.Int64
	generationErrors   atomic.Int64
	stageFailed        atomic.Int64
	activateFailed     atomic.Int64
	idemReplay         atomic.Int64
	idemConflict       atomic.Int64
	outboxDelivered    atomic.Int64
	outboxFailed       atomic.Int64
	outboxBacklog      atomic.Int64
	scannerRepairs     atomic.Int64
	scannerAlerts      atomic.Int64

	alertMu sync.Mutex
	// Minute buckets for the publication failure-rate P0 (§37.3: >5%/5min).
	buckets [5]rateBucket
}

type rateBucket struct {
	minute time.Time
	ok     int64
	fail   int64
}

// Default returns the process-wide registry.
func Default() *Registry { return global }

func (r *Registry) IncGenerationRequests() { r.generationRequests.Add(1) }
func (r *Registry) IncGenerationErrors()   { r.generationErrors.Add(1) }
func (r *Registry) IncStageFailed()        { r.stageFailed.Add(1) }
func (r *Registry) IncActivateFailed()     { r.activateFailed.Add(1) }
func (r *Registry) IncIdemReplay()         { r.idemReplay.Add(1) }
func (r *Registry) IncIdemConflict()       { r.idemConflict.Add(1) }
func (r *Registry) IncOutboxDelivered()    { r.outboxDelivered.Add(1) }
func (r *Registry) IncOutboxFailed()       { r.outboxFailed.Add(1) }
func (r *Registry) SetOutboxBacklog(n int64) {
	r.outboxBacklog.Store(n)
}
func (r *Registry) AddScannerRepairs(n int64) { r.scannerRepairs.Add(n) }
func (r *Registry) AddScannerAlerts(n int64)   { r.scannerAlerts.Add(n) }

// Snapshot returns every counter value keyed by spec metric name.
func (r *Registry) Snapshot() map[string]int64 {
	return map[string]int64{
		MetricGenerationRequests: r.generationRequests.Load(),
		MetricGenerationErrors:   r.generationErrors.Load(),
		MetricStageFailed:        r.stageFailed.Load(),
		MetricActivateFailed:     r.activateFailed.Load(),
		MetricIdemReplay:         r.idemReplay.Load(),
		MetricIdemConflict:       r.idemConflict.Load(),
		MetricOutboxDelivered:    r.outboxDelivered.Load(),
		MetricOutboxFailed:       r.outboxFailed.Load(),
		MetricOutboxBacklog:      r.outboxBacklog.Load(),
		MetricScannerRepairs:     r.scannerRepairs.Load(),
		MetricScannerAlerts:      r.scannerAlerts.Load(),
	}
}

// Reset zeroes every counter (tests only).
func (r *Registry) Reset() {
	r.generationRequests.Store(0)
	r.generationErrors.Store(0)
	r.stageFailed.Store(0)
	r.activateFailed.Store(0)
	r.idemReplay.Store(0)
	r.idemConflict.Store(0)
	r.outboxDelivered.Store(0)
	r.outboxFailed.Store(0)
	r.outboxBacklog.Store(0)
	r.scannerRepairs.Store(0)
	r.scannerAlerts.Store(0)
	r.alertMu.Lock()
	defer r.alertMu.Unlock()
	r.buckets = [5]rateBucket{}
}

// Fields are the spec §37.1 structured log fields for one publication
// lifecycle event. Zero values are omitted from the JSON line.
type Fields struct {
	RequestID       string
	Actor           string
	UserID          string
	AgentSession    string
	TemplateSlug    string
	TemplateVersion int64
	ContentID       string
	ContentVersion  int64
	PublicationID   string
	FullPath        string
	StorageProvider string
	Stage           string
	DurationMS      int64
	StatusCode      int
	ErrorCode       string
}

func (f Fields) kv() []any {
	var out []any
	add := func(k string, v any) { out = append(out, k, v) }
	if f.RequestID != "" {
		add("request_id", f.RequestID)
	}
	if f.Actor != "" {
		add("actor", f.Actor)
	}
	if f.UserID != "" {
		add("user_id", f.UserID)
	}
	if f.AgentSession != "" {
		add("agent_session", f.AgentSession)
	}
	if f.TemplateSlug != "" {
		add("template_slug", f.TemplateSlug)
	}
	if f.TemplateVersion != 0 {
		add("template_version", f.TemplateVersion)
	}
	if f.ContentID != "" {
		add("content_id", f.ContentID)
	}
	if f.ContentVersion != 0 {
		add("content_version", f.ContentVersion)
	}
	if f.PublicationID != "" {
		add("publication_id", f.PublicationID)
	}
	if f.FullPath != "" {
		add("full_path", f.FullPath)
	}
	if f.StorageProvider != "" {
		add("storage_provider", f.StorageProvider)
	}
	if f.Stage != "" {
		add("stage", f.Stage)
	}
	add("duration_ms", f.DurationMS)
	if f.StatusCode != 0 {
		add("status_code", f.StatusCode)
	}
	if f.ErrorCode != "" {
		add("error_code", f.ErrorCode)
	}
	return out
}

// LogPublication emits one structured JSON line for a publication lifecycle
// event (request received, stage/activation completed or failed).
func LogPublication(msg string, f Fields) {
	jsonlog.Default.Info("publication."+msg, f.kv()...)
}

// LogGeneration emits one structured JSON line for a page-generation
// request outcome.
func LogGeneration(msg string, f Fields) {
	jsonlog.Default.Info("page_generation."+msg, f.kv()...)
}

// AlertP0 connects a P0 condition (spec §37.3) to the existing operational
// surface: a greppable process-log line plus a structured error line.
func AlertP0(code, msg string, f Fields) {
	kv := append([]any{"code", code}, f.kv()...)
	log.Printf("[P0-ALERT] %s: %s", code, msg)
	jsonlog.Default.Error("p0."+code+" "+msg, kv...)
}

// ObservePublicationOutcome records one finished publish attempt for the
// failure-rate P0 (§37.3: failure > 5% / 5 min) and fires the alert on
// breach. ok=false counts a failure; successes keep the denominator.
func (r *Registry) ObservePublicationOutcome(ok bool) {
	r.alertMu.Lock()
	defer r.alertMu.Unlock()
	now := time.Now().UTC().Truncate(time.Minute)
	var b *rateBucket
	for i := range r.buckets {
		if r.buckets[i].minute.Equal(now) {
			b = &r.buckets[i]
			break
		}
	}
	if b == nil {
		// Evict the oldest bucket and reuse it.
		b = &r.buckets[0]
		for i := range r.buckets {
			if r.buckets[i].minute.Before(b.minute) {
				b = &r.buckets[i]
			}
		}
		*b = rateBucket{minute: now}
	}
	if ok {
		b.ok++
	} else {
		b.fail++
	}
	var okN, failN int64
	for i := range r.buckets {
		if now.Sub(r.buckets[i].minute) < 5*time.Minute {
			okN += r.buckets[i].ok
			failN += r.buckets[i].fail
		}
	}
	total := okN + failN
	if total >= 20 && float64(failN)/float64(total) > 0.05 {
		// Breach: alert once per observing call is too noisy — the process
		// log carries one line per breach window evaluation while failing.
		// (Callers invoke this once per attempt; sustained failure pages
		// repeatedly, which is the intended P0 paging behavior.)
		log.Printf("[P0-ALERT] publication_failure_rate: %d/%d failed in the last 5 minutes", failN, total)
	}
}
