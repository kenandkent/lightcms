package idempotency

// Task 17B coverage-gap tests for internal/product/idempotency.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestCoverGapOptionsAndModelHelpers(t *testing.T) {
	if _, err := NewService(nil, Options{TTLHours: 100}); err == nil {
		t.Fatalf("TTLHours 100: want error")
	}
	if _, err := NewService(nil, Options{TTLHours: -3}); err == nil {
		t.Fatalf("TTLHours negative: want error")
	}
	if _, err := NewService(nil, Options{TTLHours: 24, LeaseMinutes: -2}); err == nil {
		t.Fatalf("LeaseMinutes negative: want error")
	}
	svc, _ := newTestService(t)
	if svc.TTL() != 24*time.Hour {
		t.Fatalf("TTL() = %v", svc.TTL())
	}
	if svc.Lease() != 5*time.Minute {
		t.Fatalf("Lease() = %v", svc.Lease())
	}
	def, err := NewService(svc.repo.DB(), Options{})
	if err != nil {
		t.Fatalf("default options: %v", err)
	}
	if def.TTL() <= 0 || def.Lease() <= 0 {
		t.Fatalf("defaults not applied: %v %v", def.TTL(), def.Lease())
	}

	e := &Error{Code: CodeConflict, Message: "m"}
	if e.Error() == "" {
		t.Fatalf("Error() empty")
	}
	if CodeOf(nil) != "" || CodeOf(e) != CodeConflict || CodeOf(errors.New("x")) != "" {
		t.Fatalf("CodeOf branches")
	}
	if got := NewRepository(svc.repo.DB()).DB(); got == nil {
		t.Fatalf("Repository.DB() nil")
	}

	if isDupKey(nil) {
		t.Fatalf("isDupKey(nil)")
	}
	if !isDupKey(mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000}}}) {
		t.Fatalf("isDupKey(11000)")
	}
	if !isDupKey(errors.New("E11000 duplicate key")) {
		t.Fatalf("isDupKey(E11000 string)")
	}
	if isDupKey(errors.New("timeout")) {
		t.Fatalf("isDupKey(other)")
	}

	if itoa(0) != "0" || itoa(42) != "42" || itoa(-7) != "-7" || itoa(500) != "500" {
		t.Fatalf("itoa branches")
	}
	if httpLikeCode(503) != "HTTP_503" {
		t.Fatalf("httpLikeCode = %q", httpLikeCode(503))
	}
}

func TestCoverGapBeginValidation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	for _, tc := range [][4]string{
		{"", "POST", "/p", "k"},
		{"o", "", "/p", "k"},
		{"o", "POST", "", "k"},
		{"o", "POST", "/p", ""},
		{"  ", "POST", "/p", "k"},
	} {
		if _, err := svc.Begin(ctx, tc[0], tc[1], tc[2], tc[3], []byte("{}")); CodeOf(err) != CodeInvalidRequest {
			t.Errorf("Begin(%q): %v, want INVALID_REQUEST", tc, err)
		}
	}
}

func TestCoverGapTakeOverByKeyMatrix(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.TakeOverByKey(ctx, "o", "POST", "/p", "missing"); CodeOf(err) != CodeNotFound {
		t.Fatalf("TakeOverByKey missing: %v", err)
	}
	op := mustBegin(t, svc, "owner-take", "key-take", []byte(`{"a":1}`))
	// Live lease → TakeOver refuses (lease still valid).
	if _, err := svc.TakeOverByKey(ctx, "owner-take", "POST", "/api/v1/page-generation", "key-take"); CodeOf(err) != CodeLeaseActive {
		t.Fatalf("TakeOverByKey live lease: %v", err)
	}
	// Expired lease → takeover succeeds with bumped generation.
	expireLease(t, svc, op.ID)
	taken, err := svc.TakeOverByKey(ctx, "owner-take", "POST", "/api/v1/page-generation", "key-take")
	if err != nil {
		t.Fatalf("TakeOverByKey expired: %v", err)
	}
	if taken.LeaseGeneration != op.LeaseGeneration+1 {
		t.Fatalf("generation = %d, want %d", taken.LeaseGeneration, op.LeaseGeneration+1)
	}
	// Completed operation replays.
	if _, err := svc.Complete(ctx, op.ID, taken.Attempt, 200, map[string]any{"ok": true}, false); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	hit, err := svc.TakeOverByKey(ctx, "owner-take", "POST", "/api/v1/page-generation", "key-take")
	if err != nil || !hit.Replay {
		t.Fatalf("TakeOverByKey completed: replay=%v err=%v", hit.Replay, err)
	}
}

func TestCoverGapTakeOverMatrix(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.TakeOver(ctx, primitive.NewObjectID(), 1); CodeOf(err) != CodeNotFound {
		t.Fatalf("TakeOver missing: %v", err)
	}
	op := mustBegin(t, svc, "owner-to", "key-to", []byte(`{}`))
	if _, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration); CodeOf(err) != CodeLeaseActive {
		t.Fatalf("TakeOver live: %v", err)
	}
	if _, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration+9); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("TakeOver wrong generation: %v", err)
	}
	if _, err := svc.MarkTerminal(ctx, op.ID, op.Attempt, "RENDER_FAILED"); err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}
	if _, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("TakeOver terminal: %v", err)
	}
	op2 := mustBegin(t, svc, "owner-to2", "key-to2", []byte(`{}`))
	if _, err := svc.Complete(ctx, op2.ID, op2.Attempt, 200, nil, false); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := svc.TakeOver(ctx, op2.ID, op2.LeaseGeneration); CodeOf(err) != CodeAlreadyCompleted {
		t.Fatalf("TakeOver completed: %v", err)
	}
}

func TestCoverGapRenewLeaseLossReasons(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.RenewLease(ctx, primitive.NewObjectID(), 1, 1); CodeOf(err) != CodeNotFound {
		t.Fatalf("RenewLease missing: %v", err)
	}
	op := mustBegin(t, svc, "owner-rl", "key-rl", []byte(`{}`))
	if _, err := svc.RenewLease(ctx, op.ID, op.Attempt+5, op.LeaseGeneration); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("RenewLease wrong attempt: %v", err)
	}
	if _, err := svc.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration+5); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("RenewLease wrong generation: %v", err)
	}
	if _, err := svc.Complete(ctx, op.ID, op.Attempt, 200, nil, false); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := svc.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration); CodeOf(err) != CodeAlreadyCompleted {
		t.Fatalf("RenewLease completed: %v", err)
	}
	// Happy-path heartbeat still works.
	op2 := mustBegin(t, svc, "owner-rl2", "key-rl2", []byte(`{}`))
	if _, err := svc.RenewLease(ctx, op2.ID, op2.Attempt, op2.LeaseGeneration); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
}

func TestCoverGapBindFreezeValidationAndConflicts(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	cid := primitive.NewObjectID()
	op := mustBegin(t, svc, "owner-bf", "key-bf", []byte(`{}`))

	if err := svc.BindContentAndVersion(ctx, nil, primitive.NilObjectID, cid, 1, "/x"); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Bind zero opID: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, primitive.NilObjectID, 1, "/x"); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Bind zero contentID: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, cid, 0, "/x"); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Bind version 0: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, cid, 1, "  "); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Bind empty path: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, primitive.NewObjectID(), cid, 1, "/x"); CodeOf(err) != CodeNotFound {
		t.Fatalf("Bind missing op: %v", err)
	}
	pub := primitive.NewObjectID()
	tpl := primitive.NewObjectID()
	if err := svc.FreezeExecution(ctx, primitive.NilObjectID, 1, pub, time.Now(), tpl); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Freeze zero IDs: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, 1, pub, time.Time{}, tpl); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("Freeze zero time: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, 1, pub, time.Now(), tpl); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, op.Attempt+3, pub, time.Now(), tpl); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("Freeze wrong attempt: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, 1, primitive.NewObjectID(), time.Now(), tpl); CodeOf(err) != CodeConflict {
		t.Fatalf("Freeze different publication: %v", err)
	}
	if err := svc.FreezeExecution(ctx, primitive.NewObjectID(), 1, pub, time.Now(), tpl); CodeOf(err) != CodeNotFound {
		t.Fatalf("Freeze missing op: %v", err)
	}
	// Bind a different content over a bound op conflicts.
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, cid, 1, "/x"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, primitive.NewObjectID(), 1, "/x"); CodeOf(err) != CodeConflict {
		t.Fatalf("Bind different content: %v", err)
	}
	// Completed op: bind + freeze report already-completed.
	if _, err := svc.Complete(ctx, op.ID, op.Attempt, 200, nil, false); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, cid, 1, "/x"); CodeOf(err) != CodeAlreadyCompleted {
		t.Fatalf("Bind completed: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, 1, pub, time.Now(), tpl); CodeOf(err) != CodeAlreadyCompleted {
		t.Fatalf("Freeze completed: %v", err)
	}
	// Terminal attempt: bind reports stale attempt.
	op2 := mustBegin(t, svc, "owner-bf2", "key-bf2", []byte(`{}`))
	if _, err := svc.MarkTerminal(ctx, op2.ID, op2.Attempt, "X"); err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op2.ID, cid, 1, "/x"); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("Bind terminal: %v", err)
	}
}

func TestCoverGapCompleteAndMarkTerminalBlockers(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Complete(ctx, primitive.NewObjectID(), 1, 500, nil, false); CodeOf(err) != CodeNotFound {
		t.Fatalf("Complete missing: %v", err)
	}
	op := mustBegin(t, svc, "owner-cb", "key-cb", []byte(`{}`))
	if _, err := svc.Complete(ctx, op.ID, op.Attempt+7, 500, nil, false); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("Complete wrong attempt: %v", err)
	}
	if _, err := svc.Complete(ctx, op.ID, op.Attempt, 200, map[string]any{"ok": 1}, false); err != nil {
		t.Fatalf("Complete 200: %v", err)
	}
	if _, err := svc.Complete(ctx, op.ID, op.Attempt, 500, nil, false); CodeOf(err) != CodeAlreadyCompleted {
		t.Fatalf("Complete after completed: %v", err)
	}
	if _, err := svc.Complete(ctx, op.ID, op.Attempt+7, 200, nil, false); CodeOf(err) != CodeAlreadyCompleted {
		t.Fatalf("Complete wrong attempt on completed op: %v", err)
	}
	op3 := mustBegin(t, svc, "owner-cb3", "key-cb3", []byte(`{}`))
	if _, err := svc.Complete(ctx, op3.ID, op3.Attempt+7, 500, nil, false); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("Complete superseded: %v", err)
	}
	if _, err := svc.MarkTerminal(ctx, op.ID, op.Attempt, ""); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("MarkTerminal empty code: %v", err)
	}
	if _, err := svc.MarkTerminal(ctx, primitive.NewObjectID(), 1, "X"); CodeOf(err) != CodeNotFound {
		t.Fatalf("MarkTerminal missing: %v", err)
	}
	op2 := mustBegin(t, svc, "owner-cb2", "key-cb2", []byte(`{}`))
	if _, err := svc.MarkTerminal(ctx, op2.ID, op2.Attempt+4, "X"); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("MarkTerminal wrong attempt: %v", err)
	}
}

func TestCoverGapGetAndRepoErrors(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Get(ctx, primitive.NewObjectID()); CodeOf(err) != CodeNotFound {
		t.Fatalf("Get missing: %v", err)
	}
	op := mustBegin(t, svc, "owner-get", "key-get", []byte(`{}`))
	if _, err := svc.Get(ctx, op.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Transport errors surface raw through Get and read paths.
	bdb := testutil.MustConnectBrokenDB(t)
	bsvc, err := NewService(bdb, Options{})
	if err != nil {
		t.Fatalf("broken NewService: %v", err)
	}
	if _, err := bsvc.Get(ctx, op.ID); err == nil {
		t.Fatalf("broken Get: want error")
	}
	if _, err := bsvc.TakeOverByKey(ctx, "o", "POST", "/p", "k"); err == nil {
		t.Fatalf("broken TakeOverByKey: want error")
	}
	if _, err := bsvc.Begin(ctx, "o", "POST", "/p", "k", []byte("{}")); err == nil {
		t.Fatalf("broken Begin insert: want error")
	}
}
