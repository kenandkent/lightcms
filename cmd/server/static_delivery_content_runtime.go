package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"log"
	"strings"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/handlers"
	"github.com/metavert-io/lightcms/v7/internal/models"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	cf "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"
	deliveryapi "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/httpapi"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type deliveryAssetLookup struct{ db *database.DB }

func (l deliveryAssetLookup) LookupAsset(ctx context.Context, path string) (ex.AssetSnapshot, error) {
	var asset models.Asset
	e := l.db.Collection("assets").FindOne(ctx, bson.M{"$or": bson.A{bson.M{"full_path": path}, bson.M{"serve_path": path}}}).Decode(&asset)
	if errors.Is(e, mongo.ErrNoDocuments) {
		return ex.AssetSnapshot{}, nil
	}
	if e != nil {
		return ex.AssetSnapshot{}, e
	}
	return ex.AssetSnapshot{Data: asset.Data, MIME: asset.MimeType, Revision: asset.ID.Hex() + ":" + asset.UpdatedAt.UTC().String()}, nil
}
func deliveryActorAuthorization(db *database.DB) func(context.Context, sd.ActorSnapshot, bool, bool) error {
	return func(ctx context.Context, a sd.ActorSnapshot, publish, recovery bool) error {
		// Recovery only inspects remote state and records an already-issued action.
		if recovery {
			return nil
		}
		denied := func() error {
			return &sd.Error{Code: sd.CodePermissionDenied, Message: "原操作者或API凭证已失效，任务已阻塞"}
		}
		id, e := primitive.ObjectIDFromHex(a.UserID)
		if e != nil {
			return denied()
		}
		var user models.User
		if e = db.Collection("users").FindOne(ctx, bson.M{"_id": id}).Decode(&user); e != nil || user.Disabled {
			return denied()
		}
		if a.Role == "admin" && user.Role != "admin" {
			return denied()
		}
		current := &auth.SessionUser{ID: a.UserID, Role: user.Role, Scopes: a.Scopes}
		frozen := &auth.SessionUser{ID: a.UserID, Role: a.Role, Scopes: a.Scopes}
		perms := []string{auth.PermContentView}
		if publish {
			perms = append(perms, auth.PermContentPublish)
		}
		for _, p := range perms {
			if !auth.UserHasPermission(frozen, p) || !auth.UserHasPermission(current, p) {
				return denied()
			}
		}
		if strings.HasPrefix(a.Owner, "apikey:") {
			kid, e := primitive.ObjectIDFromHex(strings.TrimPrefix(a.Owner, "apikey:"))
			if e != nil {
				return denied()
			}
			var key models.APIKey
			if e = db.Collection("api_keys").FindOne(ctx, bson.M{"_id": kid}).Decode(&key); e != nil || key.UserID == nil || *key.UserID != id || key.SandboxOnly {
				return denied()
			}
			current.Scopes = key.Scopes
			for _, p := range perms {
				if !auth.UserHasPermission(current, p) {
					return denied()
				}
			}
		} else if a.Owner != a.UserID {
			return denied()
		} // Unknown credential owners fail closed.
		return nil
	}
}
func wireStaticDeliveryContent(ctx, bg context.Context, admin *handlers.StaticDeliveryAdmin, db *database.DB, cfg sd.Config, pubs *publicationRuntime, origin string, workflows ...*jobs.Runner) error {
	if admin == nil || admin.Unavailable != "" || pubs == nil {
		return fmt.Errorf("static delivery dependencies unavailable")
	}
	source := ex.NewPublicationSource(staticDeliverySourceLookup{db}, pubs.PubRepo, pubs.Store)
	resolver, e := ex.NewFileResolver(cfg.ProjectRoot, origin, deliveryAssetLookup{db})
	if e != nil {
		return e
	}
	objectStore := sd.NewObjectReleaseStore(cfg, db)
	if err := objectStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("static artifact indexes unavailable: %w", err)
	}
	exporter := ex.NewService(source, resolver, ex.Options{ProjectRoot: cfg.ProjectRoot, Root: cfg.Root, Objects: objectStore})
	defaults := func(ctx context.Context, slug string) (ex.Resource, ex.Resource, error) {
		prefix := "/static/images/report-templates/" + slug
		switch slug {
		case "crypto-analysis":
			prefix = "/static/images/chain-lens"
		case "cybersecurity-report", "fund-research", "venture-research", "binance-announcement-style", "okx-announcement-style", "financial-daily", "editorial-news", "technology-report":
		default:
			return ex.Resource{}, ex.Resource{}, nil
		}
		share, e := resolver.Resolve(ctx, prefix+"-share.png")
		if e != nil {
			return ex.Resource{}, ex.Resource{}, e
		}
		icon, e := resolver.Resolve(ctx, prefix+"-icon.svg")
		return share, icon, e
	}
	repo := sd.NewRepository(db)
	budget := cfg.MemoryBudgetBytes
	if budget <= 0 {
		budget = 1 << 30
	}
	admin.Artifacts = objectStore
	capacity := artifacts.NewAdmission(budget)
	admin.Capacity = capacity
	verifier := sd.NewReleaseVerifier()
	commands := sd.NewCommandService(repo, sd.CommandOptions{InspectTarget: admin.Targets.InspectTarget, VerifyRecovery: verifier.Check, CheckCandidateOrigin: admin.Targets.CheckCandidateOrigin, Admission: capacity, Source: source, Exports: exporter, Store: objectStore, Accounts: admin.Accounts, Authorize: deliveryActorAuthorization(db), Defaults: defaults})
	if len(workflows) > 0 && workflows[0] != nil {
		commands.EnablePreviewJobs(workflows[0])
	}
	runner := commands.NewRunner(sd.RunnerDependencies{Owner: "delivery-" + primitive.NewObjectID().Hex(), Concurrency: cfg.GlobalConcurrency, Timeout: cfg.JobTimeout, VerifyBatch: verifier.CheckBatch, Remote: func(ctx context.Context, j sd.Job, t sd.Target) (sd.Remote, error) {
		var cred sd.Credential
		if e := db.Collection("static_delivery_credentials").FindOne(ctx, bson.M{"_id": j.CredentialRef, "account_ref": t.AccountRef}).Decode(&cred); e != nil {
			return nil, &sd.Error{Code: sd.CodeCredentialUnavailable, Message: "固定凭证版本不可用"}
		}
		account, e := admin.Accounts.Get(ctx, t.AccountRef)
		if e != nil {
			return nil, e
		}
		if j.Checkpoint.ActivationIntentAt == nil && account.Status != "enabled" {
			return nil, &sd.Error{Code: sd.CodeAccountDisabled, Message: "账号已停用"}
		}
		raw, e := admin.Accounts.Token(ctx, t.AccountRef, cred.Revision)
		if e != nil {
			return nil, e
		}
		return &sd.CloudflareRemote{Client: cf.NewClient(string(raw), cf.Options{})}, nil
	}})
	admin.Commands = commands
	admin.ContentAPI = &deliveryapi.Handlers{Commands: commands, Exports: exporter}
	go func() {
		ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		if err := commands.RebuildPageStatus(ctx); err != nil {
			log.Printf("delivery page status rebuild: %v", err)
		}
	}()
	go runner.Run(bg)
	return nil
}
