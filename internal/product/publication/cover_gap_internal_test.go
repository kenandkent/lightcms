package publication

// Task 17B coverage-gap tests: unexported helpers + pure branches (in-package).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/observe"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestCoverGapModelHelpers(t *testing.T) {
	withMsg := &Error{Code: CodeConflict, Message: "m", Err: errors.New("cause")}
	if got := withMsg.Error(); got == "" {
		t.Fatalf("Error() empty")
	}
	bare := &Error{Code: CodeNotFound}
	if got := bare.Error(); got == "" {
		t.Fatalf("Error() bare empty")
	}
	if bare.Unwrap() != nil || withMsg.Unwrap() == nil {
		t.Fatalf("Unwrap branches")
	}
	if CodeOf(nil) != "" || CodeOf(withMsg) != CodeConflict || CodeOf(errors.New("x")) != "" {
		t.Fatalf("CodeOf branches")
	}
	if IsConflict(nil) {
		t.Fatalf("IsConflict(nil)")
	}
	if !IsConflict(withMsg) {
		t.Fatalf("IsConflict(typed)")
	}
	dup := mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000}}}
	if !IsConflict(dup) || !isDupKey(dup) {
		t.Fatalf("IsConflict/isDupKey(11000)")
	}
	if !isDupKey(errors.New("duplicate key")) {
		t.Fatalf("isDupKey(string)")
	}
	if isDupKey(nil) || isDupKey(errors.New("timeout")) || IsConflict(errors.New("timeout")) {
		t.Fatalf("isDupKey/IsConflict negative")
	}
	_ = pubErr(CodeValidation, "x", nil)

	var nilPub *Publication
	if nilPub.IsServable() {
		t.Fatalf("nil IsServable")
	}

	if CodeOfRender(nil) != "" {
		t.Fatalf("CodeOfRender(nil)")
	}
	if CodeOfRender(renderErr(CodeRenderValidation, "v", nil)) != CodeRenderValidation {
		t.Fatalf("CodeOfRender(validation)")
	}
	if CodeOfRender(renderErr("OTHER", "o", nil)) != "" {
		t.Fatalf("CodeOfRender(other)")
	}
	if CodeOfRender(errors.New("plain")) != "" {
		t.Fatalf("CodeOfRender(plain)")
	}
	_ = renderErr(CodeRenderUnsafe, "u", nil).Error()
}

func TestCoverGapNormalizeAndAllowUnsafe(t *testing.T) {
	if normalizeScriptPolicy("") != "" || normalizeScriptPolicy("all") != "all" ||
		normalizeScriptPolicy("admin_only") != "admin_only" || normalizeScriptPolicy("none") != "none" ||
		normalizeScriptPolicy("bogus") != "" {
		t.Fatalf("normalizeScriptPolicy branches")
	}
	if ok, err := resolveAllowUnsafe("", false); !ok || err != nil {
		t.Fatalf("inherit: %v %v", ok, err)
	}
	if ok, _ := resolveAllowUnsafe("all", false); !ok {
		t.Fatalf("all")
	}
	if ok, _ := resolveAllowUnsafe("none", false); ok {
		t.Fatalf("none")
	}
	if ok, _ := resolveAllowUnsafe("admin_only", true); !ok {
		t.Fatalf("admin_only admin")
	}
	if ok, _ := resolveAllowUnsafe("admin_only", false); ok {
		t.Fatalf("admin_only editor")
	}
	// Unknown policy normalizes to inherit (allowed): the switch default is
	// unreachable because normalize only yields "", all, admin_only, none.
	if ok, err := resolveAllowUnsafe("bogus", false); !ok || err != nil {
		t.Fatalf("unknown policy inherit: %v %v", ok, err)
	}
}

func validGapSnapshot() RenderSnapshot {
	return RenderSnapshot{
		PublicationID: primitive.NewObjectID(), ContentID: primitive.NewObjectID(),
		ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(), TemplateVersion: 3,
		FullPath: "/news/x", HTMLLayout: "<p>{{.headline}}</p>",
		LogicalPublishedAt: time.Now(), PublicURL: "https://example.com/news/x",
		Data: map[string]any{"headline": "h"},
	}
}

func TestCoverGapValidateSnapshot(t *testing.T) {
	if err := validateSnapshot(nil); err == nil {
		t.Fatalf("nil snapshot: want error")
	}
	base := validGapSnapshot()
	if err := validateSnapshot(&base); err != nil {
		t.Fatalf("valid: %v", err)
	}
	cases := []func(*RenderSnapshot){
		func(s *RenderSnapshot) { s.PublicationID = primitive.NilObjectID },
		func(s *RenderSnapshot) { s.ContentID = primitive.NilObjectID },
		func(s *RenderSnapshot) { s.ContentVersion = 0 },
		func(s *RenderSnapshot) { s.TemplateVersionID = primitive.NilObjectID },
		func(s *RenderSnapshot) { s.TemplateVersion = 0 },
		func(s *RenderSnapshot) { s.FullPath = "  " },
		func(s *RenderSnapshot) { s.FullPath = "relative" },
		func(s *RenderSnapshot) { s.HTMLLayout = "" },
		func(s *RenderSnapshot) { s.LogicalPublishedAt = time.Time{} },
		func(s *RenderSnapshot) { s.PublicURL = "" },
		func(s *RenderSnapshot) { s.ScriptPolicy = "bogus" },
	}
	for i, mut := range cases {
		s := validGapSnapshot()
		mut(&s)
		if err := validateSnapshot(&s); err == nil {
			t.Fatalf("case %d: want error", i)
		}
	}
	nilData := validGapSnapshot()
	nilData.Data = nil
	nilData.ScriptPolicy = "none"
	if err := validateSnapshot(&nilData); err != nil {
		t.Fatalf("nil data init: %v", err)
	}
	if nilData.Data == nil {
		t.Fatalf("nil data not initialized")
	}
}

func TestCoverGapDefaultBackoff(t *testing.T) {
	if defaultBackoff(0) != 30_000_000_000 {
		t.Fatalf("attempt 0")
	}
	if defaultBackoff(1) != 30_000_000_000 || defaultBackoff(2) != 5*60_000_000_000 {
		t.Fatalf("attempts 1-2")
	}
	if defaultBackoff(3) != 30*60_000_000_000 || defaultBackoff(9) != 3_600_000_000_000 {
		t.Fatalf("attempts 3+")
	}
}

func TestCoverGapScannerOptReaders(t *testing.T) {
	s := &Scanner{}
	if s.retentionDays() != DefaultRetentionDays || s.failedRetentionDays() != DefaultFailedRetentionDays ||
		s.quarantineRetentionDays() != DefaultQuarantineRetentionDays || s.outboxMaxAttempts() != DefaultOutboxMaxAttempts ||
		s.outboxMaxAge() != DefaultOutboxMaxAge || s.outboxTerminalRetentionDays() != DefaultOutboxTerminalRetentionDays ||
		s.scanInterval() != DefaultScanInterval {
		t.Fatalf("default readers")
	}
	if s.stageTimeout() != storage.DefaultStageTimeout {
		t.Fatalf("default stageTimeout")
	}
	c := &Scanner{opts: ScannerOptions{
		RetentionDays: 1, FailedRetentionDays: 2, QuarantineRetentionDays: 3,
		OutboxMaxAttempts: 4, OutboxMaxAge: time.Hour, OutboxTerminalRetentionDays: 5,
		ScanInterval: time.Second, StageTimeout: time.Minute, QuarantineDir: "/q",
	}}
	if c.retentionDays() != 1 || c.failedRetentionDays() != 2 || c.quarantineRetentionDays() != 3 ||
		c.outboxMaxAttempts() != 4 || c.outboxMaxAge() != time.Hour || c.outboxTerminalRetentionDays() != 5 ||
		c.scanInterval() != time.Second || c.stageTimeout() != time.Minute || c.quarantineDir() != "/q" {
		t.Fatalf("custom readers")
	}
	fsStore := storage.NewFilesystemStore(t.TempDir())
	f := &Scanner{store: fsStore}
	if f.quarantineDir() == "" {
		t.Fatalf("filesystem quarantine default empty")
	}
	if (&Scanner{}).quarantineDir() != "" {
		t.Fatalf("nil-store quarantine")
	}
	if s.Degraded() {
		t.Fatalf("Degraded initially true")
	}
	s.ResetDegraded()
	if s.Degraded() {
		t.Fatalf("ResetDegraded")
	}
}

func TestCoverGapObserveScan(t *testing.T) {
	s := &Scanner{}
	// Double-Run early return: latch already held.
	s.running.Store(true)
	s.Run(context.Background())
	s.running.Store(false)

	// No quarantine directory (empty-root store) → refused operation error.
	rootless := &Scanner{store: storage.NewFilesystemStore(""), opts: ScannerOptions{}}
	if _, err := rootless.quarantineFile(&scanState{}, "/absent/x.html", "test"); err == nil {
		t.Fatalf("quarantineFile without dir: want error")
	} else if err.Error() == "" {
		t.Fatalf("scannerError empty")
	}
	if errNoQuarantine("q").Error() == "" {
		t.Fatalf("errNoQuarantine empty")
	}
	before := observe.Default().Snapshot()
	s.observeScan(ScanReport{})
	s.observeScan(ScanReport{
		CanonicalRebuilt: 1, CanonicalMismatchRepaired: 1, PreviousRestored: 1,
		PreviousCleaned: 1, BackupRestored: 1, BackupCleaned: 1, StaleNextCleaned: 1,
		StaleStagedFailed: 1, OrphanQuarantined: 1,
		Alerts:             []Alert{{Severity: "WARN", Code: "X"}},
		ImmutableMissingP0: 1, ImmutableCorruptP0: 2, MultipleActiveP0: 0,
	})
	after := observe.Default().Snapshot()
	if after[observe.MetricScannerRepairs] < before[observe.MetricScannerRepairs]+9 {
		t.Fatalf("repairs not counted: %+v", after)
	}
	if after[observe.MetricScannerAlerts] < before[observe.MetricScannerAlerts]+1 {
		t.Fatalf("alerts not counted")
	}
}
