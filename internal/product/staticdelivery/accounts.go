package staticdelivery

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type AccountProbe func(context.Context, string, string, string) (ConnectionCheck, error)
type AccountService struct {
	repo  *Repository
	vault *Vault
	probe AccountProbe
}

func NewAccountService(r *Repository, v *Vault, p AccountProbe) *AccountService {
	return &AccountService{r, v, p}
}
func newRecord() Record {
	now := time.Now().UTC()
	return Record{ID: primitive.NewObjectID(), SchemaVersion: SchemaVersion, CreatedAt: now, UpdatedAt: now}
}

var cfAccountID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *AccountService) Create(ctx context.Context, name, account, token, remark string) (Account, error) {
	account = strings.ToLower(strings.TrimSpace(account))
	name = strings.TrimSpace(name)
	if !cfAccountID.MatchString(account) || name == "" || len(name) > 200 || strings.TrimSpace(token) == "" || len(token) > 8192 || strings.ContainsAny(token, "\r\n") {
		return Account{}, failure(CodeInvalidRequest, "account ID, name and token required", nil)
	}
	a := Account{Record: newRecord(), Name: name, AccountID: account, Status: "enabled", Remark: remark, CredentialRevision: 1}
	credential := Credential{Record: newRecord(), AccountRef: a.ID, Revision: 1, Token: token, Status: "active"}
	err := s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, e := s.repo.collection("accounts").InsertOne(sc, a); e != nil {
			return e
		}
		_, e := s.repo.collection("credentials").InsertOne(sc, credential)
		return e
	})
	if mongo.IsDuplicateKeyError(err) {
		return Account{}, failure(CodeAccountExists, "Cloudflare account already exists; restore the existing account", nil)
	}
	return a, err
}
func (s *AccountService) Get(ctx context.Context, id primitive.ObjectID) (Account, error) {
	var a Account
	e := s.repo.collection("accounts").FindOne(ctx, bson.M{"_id": id}).Decode(&a)
	if errors.Is(e, mongo.ErrNoDocuments) {
		e = failure(CodeNotFound, "account not found", nil)
	}
	return a, e
}
func (s *AccountService) List(ctx context.Context) ([]Account, error) {
	cur, e := s.repo.collection("accounts").Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(1000))
	if e != nil {
		return nil, e
	}
	defer cur.Close(ctx)
	out := []Account{}
	e = cur.All(ctx, &out)
	return out, e
}

// Token deliberately permits disabled accounts for read-only reconciliation.
// Every mutation separately checks the current account and target status.
func (s *AccountService) Token(ctx context.Context, id primitive.ObjectID, revision int64) ([]byte, error) {
	var c Credential
	e := s.repo.collection("credentials").FindOne(ctx, bson.M{"account_ref": id, "revision": revision}).Decode(&c)
	if e != nil {
		return nil, failure(CodeCredentialUnavailable, "credential revision unavailable", nil)
	}
	if c.Token != "" {
		return []byte(c.Token), nil
	}
	// Legacy encrypted revisions are never treated as plaintext. An optional
	// old key permits reading them; otherwise rotate by entering a new token.
	return s.vault.Open(id, revision, Sealed{Ciphertext: c.Ciphertext, Nonce: c.Nonce, KeyID: c.KeyID})
}
func (s *AccountService) check(ctx context.Context, token string, a Account, worker string) (ConnectionCheck, error) {
	if s.probe == nil {
		return ConnectionCheck{}, failure(CodeModuleUnavailable, "Cloudflare connection checks unavailable", nil)
	}
	check, e := s.probe(ctx, token, a.AccountID, worker)
	check.CheckedAt = time.Now().UTC()
	check.WriteVerified = false
	return check, e
}
func (s *AccountService) Verify(ctx context.Context, id primitive.ObjectID, worker string) (ConnectionCheck, error) {
	a, e := s.Get(ctx, id)
	if e != nil {
		return ConnectionCheck{}, e
	}
	raw, e := s.Token(ctx, id, a.CredentialRevision)
	if e != nil {
		return ConnectionCheck{}, e
	}
	check, probeErr := s.check(ctx, string(raw), a, worker)
	check.WriteVerified = a.Verification.WriteVerified
	if probeErr != nil {
		check.Readable = false
		check.Code = CodeCFPermission
	}
	res, e := s.repo.collection("accounts").UpdateOne(ctx, bson.M{"_id": id, "credential_revision": a.CredentialRevision}, bson.M{"$set": bson.M{"verification": check, "updated_at": time.Now().UTC()}})
	if e != nil {
		return check, e
	}
	if res.MatchedCount != 1 {
		return check, failure(CodeRevisionConflict, "credential changed during check", nil)
	}
	if probeErr != nil {
		return check, remoteError(probeErr, CodeCFUnavailable)
	}
	return check, nil
}
func (s *AccountService) Rotate(ctx context.Context, id primitive.ObjectID, token, worker string) (Account, error) {
	if strings.TrimSpace(token) == "" || len(token) > 8192 || strings.ContainsAny(token, "\r\n") {
		return Account{}, failure(CodeInvalidRequest, "nonempty replacement token required", nil)
	}
	a, e := s.Get(ctx, id)
	if e != nil {
		return a, e
	}
	if a.Status == "archived" {
		return a, failure(CodeAccountDisabled, "restore account before rotating credentials", nil)
	}
	check, e := s.check(ctx, token, a, worker)
	if e != nil || !check.Readable {
		return a, failure(CodeCFPermission, "replacement credential did not verify account access", nil)
	}
	rev := a.CredentialRevision + 1
	c := Credential{Record: newRecord(), AccountRef: id, Revision: rev, Token: token, Status: "active"}
	e = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		res, e := s.repo.collection("accounts").UpdateOne(sc, bson.M{"_id": id, "credential_revision": a.CredentialRevision, "status": bson.M{"$ne": "archived"}}, bson.M{"$set": bson.M{"credential_revision": rev, "verification": check, "updated_at": time.Now().UTC()}})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeRevisionConflict, "account changed during credential rotation", nil)
		}
		if _, e = s.repo.collection("credentials").UpdateOne(sc, bson.M{"account_ref": id, "revision": a.CredentialRevision}, bson.M{"$set": bson.M{"status": "retired"}}); e != nil {
			return e
		}
		_, e = s.repo.collection("credentials").InsertOne(sc, c)
		return e
	})
	if e != nil {
		return a, e
	}
	return s.Get(ctx, id)
}
func (s *AccountService) SetEnabled(ctx context.Context, id primitive.ObjectID, enabled bool) error {
	a, e := s.Get(ctx, id)
	if e != nil {
		return e
	}
	if a.Status == "archived" {
		return failure(CodeAccountDisabled, "restore account first", nil)
	}
	state := "disabled"
	if enabled {
		if !a.Verification.Readable {
			return failure(CodeCFPermission, "verify account access before enabling", nil)
		}
		state = "enabled"
	}
	res, e := s.repo.collection("accounts").UpdateOne(ctx, bson.M{"_id": id, "status": a.Status, "credential_revision": a.CredentialRevision}, bson.M{"$set": bson.M{"status": state, "updated_at": time.Now().UTC()}})
	if e == nil && res.MatchedCount != 1 {
		return failure(CodeRevisionConflict, "account changed", nil)
	}
	return e
}
func (s *AccountService) Archive(ctx context.Context, id primitive.ObjectID) error {
	return s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		a, e := s.Get(sc, id)
		if e != nil {
			return e
		}
		cur, e := s.repo.collection("targets").Find(sc, bson.M{"account_ref": id})
		if e != nil {
			return e
		}
		defer cur.Close(sc)
		var targets []Target
		if e = cur.All(sc, &targets); e != nil {
			return e
		}
		for _, t := range targets {
			if !t.ActiveJobID.IsZero() || t.Status == "initializing" {
				return failure(CodeTargetBusy, "account has an active target operation", nil)
			}
		}
		res, e := s.repo.collection("accounts").UpdateOne(sc, bson.M{"_id": id, "credential_revision": a.CredentialRevision}, bson.M{"$set": bson.M{"status": "archived", "updated_at": time.Now().UTC()}})
		if e == nil && res.MatchedCount != 1 {
			return failure(CodeRevisionConflict, "account changed", nil)
		}
		return e
	})
}
func (s *AccountService) Restore(ctx context.Context, id primitive.ObjectID) error {
	res, e := s.repo.collection("accounts").UpdateOne(ctx, bson.M{"_id": id, "status": "archived"}, bson.M{"$set": bson.M{"status": "disabled", "updated_at": time.Now().UTC()}})
	if e == nil && res.MatchedCount != 1 {
		return failure(CodeRevisionConflict, "account is not archived", nil)
	}
	return e
}
