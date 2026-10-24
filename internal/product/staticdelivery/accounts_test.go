package staticdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAccountLegacyEncryptedTokenRequiresExplicitReplacement(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	v, _ := NewVault(bytes.Repeat([]byte{7}, 32))
	a := Account{Record: newRecord(), AccountID: "abcdef0123456789abcdef0123456789", Name: "旧账号", Status: "enabled", CredentialRevision: 1}
	sealed, err := v.Seal(a.ID, 1, []byte("legacy-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.collection("accounts").InsertOne(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err = r.collection("credentials").InsertOne(ctx, Credential{Record: newRecord(), AccountRef: a.ID, Revision: 1, Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, KeyID: sealed.KeyID, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	s := NewAccountService(r, nil, func(context.Context, string, string, string) (ConnectionCheck, error) {
		return ConnectionCheck{Readable: true}, nil
	})
	if _, err = s.Token(ctx, a.ID, 1); CodeOf(err) != CodeCredentialUnavailable {
		t.Fatal("encrypted bytes must not be used as a token", err)
	}
	legacy := NewAccountService(r, v, nil)
	if raw, err := legacy.Token(ctx, a.ID, 1); err != nil || string(raw) != "legacy-token" {
		t.Fatal("optional legacy read failed", err)
	}
	if _, err = s.Rotate(ctx, a.ID, "replacement-token", ""); err != nil {
		t.Fatal("replacement must not require old key", err)
	}
	if raw, err := s.Token(ctx, a.ID, 2); err != nil || string(raw) != "replacement-token" {
		t.Fatal(err)
	}
	if _, err = s.Token(ctx, primitive.NewObjectID(), 1); CodeOf(err) != CodeCredentialUnavailable {
		t.Fatal("missing revision accepted")
	}
}

func TestAccountPlaintextCredentialsNeedNoMasterKey(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	s := NewAccountService(r, nil, func(context.Context, string, string, string) (ConnectionCheck, error) {
		return ConnectionCheck{Readable: true}, nil
	})
	a, err := s.Create(ctx, "免密钥账号", "abcdef0123456789abcdef0123456789", "first-token", "")
	if err != nil {
		t.Fatal("saving account must not require a server key", err)
	}
	var stored bson.M
	if err := r.collection("credentials").FindOne(ctx, bson.M{"account_ref": a.ID, "revision": int64(1)}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored["token"] != "first-token" {
		t.Fatal("token was not stored as plaintext")
	}
	if _, err = s.Rotate(ctx, a.ID, "second-token", ""); err != nil {
		t.Fatal(err)
	}
	restarted := NewAccountService(r, nil, nil)
	for revision, expected := range map[int64]string{1: "first-token", 2: "second-token"} {
		raw, err := restarted.Token(ctx, a.ID, revision)
		if err != nil || string(raw) != expected {
			t.Fatalf("revision %d unreadable without key: %v", revision, err)
		}
	}
	var c Credential
	if err = r.collection("credentials").FindOne(ctx, bson.M{"account_ref": a.ID, "revision": int64(2)}).Decode(&c); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(c)
	if err != nil || bytes.Contains(encoded, []byte("second-token")) {
		t.Fatal("credential must not be exposed in JSON", err)
	}
}

func TestAccountRotationPinsOldCredentials(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	v, _ := NewVault(bytes.Repeat([]byte{1}, 32))
	checks := 0
	s := NewAccountService(r, v, func(ctx context.Context, token, account, worker string) (ConnectionCheck, error) {
		checks++
		if token == "bad" {
			return ConnectionCheck{}, errors.New("bad-token")
		}
		return ConnectionCheck{Readable: true, ListReadable: false, Subdomain: "example"}, nil
	})
	a, e := s.Create(ctx, "生产账号", "ABCDEF0123456789ABCDEF0123456789", "first-secret", "")
	if e != nil {
		t.Fatal(e)
	}
	if a.AccountID != "abcdef0123456789abcdef0123456789" || a.CredentialRevision != 1 || a.Verification.Readable {
		t.Fatalf("bad initial account %+v", a)
	}
	if _, e = s.Create(ctx, "duplicate", a.AccountID, "x", ""); CodeOf(e) != CodeAccountExists {
		t.Fatalf("duplicate: %v", e)
	}
	if _, e = s.Rotate(ctx, a.ID, "bad", "reports"); e == nil {
		t.Fatal("unverified rotation")
	}
	a, e = s.Rotate(ctx, a.ID, "second-secret", "reports")
	if e != nil || a.CredentialRevision != 2 || !a.Verification.Readable {
		t.Fatalf("%+v %v", a, e)
	}
	old, e := s.Token(ctx, a.ID, 1)
	if e != nil || string(old) != "first-secret" {
		t.Fatal("old revision not retained", e)
	}
	current, e := s.Token(ctx, a.ID, 2)
	if e != nil || string(current) != "second-secret" {
		t.Fatal("wrong rotated token", e)
	}
	raw, _ := json.Marshal(a)
	if bytes.Contains(raw, []byte("secret")) {
		t.Fatal("credential exposed")
	}
	if e = s.SetEnabled(ctx, a.ID, false); e != nil {
		t.Fatal(e)
	}
	if checks != 2 {
		t.Fatal("disable made external request")
	}
	if e = s.Archive(ctx, a.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.Restore(ctx, a.ID); e != nil {
		t.Fatal(e)
	}
	a, e = s.Get(ctx, a.ID)
	if e != nil || a.Status != "disabled" {
		t.Fatalf("restore must not enable %+v %v", a, e)
	}
	if _, e = s.Token(ctx, a.ID, 2); e != nil {
		t.Fatal("disabled credentials required for read-only reconcile", e)
	}
	var doc bson.M
	if e = r.collection("credentials").FindOne(ctx, bson.M{"account_ref": a.ID, "revision": int64(1)}).Decode(&doc); e != nil {
		t.Fatal(e)
	}
}

type targetRemote struct {
	state                  RemoteState
	initCalls, enableCalls int
	inspectErr, initErr    error
	onInit                 func()
}

func (f *targetRemote) Inspect(context.Context, Target) (RemoteState, error) {
	return f.state, f.inspectErr
}
func (f *targetRemote) Initialize(_ context.Context, t Target, _ []Module) (RemoteVersion, error) {
	f.initCalls++
	if f.onInit != nil {
		f.onInit()
	}
	if f.initErr != nil {
		return RemoteVersion{}, f.initErr
	}
	f.state = RemoteState{Exists: true, SingleVersion: true, InstallationID: t.InstallationID, TargetID: t.ID.Hex(), VersionID: "v1", DeploymentID: "d1"}
	return RemoteVersion{ID: "v1"}, nil
}
func (*targetRemote) UploadAssets(context.Context, Target, []UploadFile) (CompletionToken, error) {
	panic("not initialization")
}
func (*targetRemote) UploadVersion(context.Context, Target, CompletionToken, []Module) (RemoteVersion, error) {
	panic("not initialization")
}
func (*targetRemote) Activate(context.Context, Target, string) (RemoteDeployment, error) {
	panic("not initialization")
}

func TestTargetInitializationIntentAndIsolation(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	v, _ := NewVault(bytes.Repeat([]byte{2}, 32))
	s := NewAccountService(r, v, func(context.Context, string, string, string) (ConnectionCheck, error) {
		return ConnectionCheck{Readable: true, Subdomain: "example"}, nil
	})
	a, _ := s.Create(ctx, "账号A", "0123456789abcdef0123456789abcdef", "a-token", "")
	s.Verify(ctx, a.ID, "reports")
	b, _ := s.Create(ctx, "账号B", "1123456789abcdef0123456789abcdef", "b-token", "")
	s.Verify(ctx, b.ID, "reports")
	f := &targetRemote{}
	ts := NewTargetService(r, s, "installation-1", func(token, marker string) Remote { return f }, func(context.Context, string, Target) error { f.enableCalls++; return nil }, func(context.Context, Target, RemoteState) error { return nil })
	target, e := ts.Create(ctx, a.ID, "测试站", "reports", "test", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ts.Create(ctx, a.ID, "duplicate", "reports", "test", ""); CodeOf(e) != CodeTargetExists {
		t.Fatal(e)
	}
	if _, e = ts.Create(ctx, b.ID, "独立站", "reports", "production", ""); e != nil {
		t.Fatal("same Worker allowed in different accounts", e)
	}
	f.onInit = func() {
		persisted, _ := r.GetTarget(ctx, target.ID)
		if persisted.Status != "initializing" || persisted.InitializationCredentialRevision != 1 {
			t.Error("no durable intent before PUT")
		}
	}
	target, e = ts.Initialize(ctx, target.ID)
	if e != nil || target.Status != "ready" || target.Origin != "https://reports.example.workers.dev" || f.initCalls != 1 {
		t.Fatalf("%+v %v", target, e)
	}
	f.onInit = nil
	verifiedAccount, _ := s.Get(ctx, a.ID)
	if !verifiedAccount.Verification.WriteVerified {
		t.Fatal("successful initialization did not record pinned credential write verification")
	}
	if e = ts.SetEnabled(ctx, target.ID, false); e != nil {
		t.Fatal(e)
	}
	if f.initCalls != 1 || f.enableCalls != 1 {
		t.Fatal("disable mutated CF")
	}
	if e = ts.Archive(ctx, target.ID); e != nil {
		t.Fatal(e)
	}
	if e = ts.Restore(ctx, target.ID); e != nil {
		t.Fatal(e)
	}
	target, _ = r.GetTarget(ctx, target.ID)
	if target.Status != "disabled" {
		t.Fatal("restore enabled target")
	}
	other, e := ts.Create(ctx, a.ID, "未知站", "occupied", "test", "")
	if e != nil {
		t.Fatal(e)
	}
	f.state = RemoteState{Exists: true, InstallationID: "someone-else"}
	if _, e = ts.Initialize(ctx, other.ID); CodeOf(e) != CodeUnmanagedWorker {
		t.Fatalf("unmanaged overwrite: %v", e)
	}
	if f.initCalls != 1 {
		t.Fatal("unmanaged Worker overwritten")
	}
	other, e = ts.Create(ctx, a.ID, "超时站", "uncertain", "test", "")
	if e != nil {
		t.Fatal(e)
	}
	f.state = RemoteState{}
	f.initErr = errors.New("request timed out")
	if _, e = ts.Initialize(ctx, other.ID); e == nil {
		t.Fatal("timeout succeeded")
	}
	if _, e = ts.Initialize(ctx, other.ID); e == nil {
		t.Fatal("unknown init must remain pending")
	}
	if f.initCalls != 2 {
		t.Fatal("unknown initialization was repeated")
	}
	still, _ := r.GetTarget(ctx, other.ID)
	if still.Status != "initializing" {
		t.Fatal(still.Status)
	}
}
