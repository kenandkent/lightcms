package main

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/handlers"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	cf "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"
)

// Additive construction only. A new-module failure never changes the original
// publication runtime or prevents existing routes from starting.
func buildStaticDeliveryAdminRuntime(ctx context.Context, db *database.DB, cfg sd.Config) *handlers.StaticDeliveryAdmin {
	unavailable := func() *handlers.StaticDeliveryAdmin {
		return &handlers.StaticDeliveryAdmin{Unavailable: sd.CodeModuleUnavailable}
	}
	if db == nil {
		return unavailable()
	}
	r := sd.NewRepository(db)
	if e := r.EnsureIndexes(ctx); e != nil {
		return unavailable()
	}
	installation, e := r.InstallationIdentity(ctx, cfg)
	if e != nil {
		return unavailable()
	}
	// New credentials are plaintext and require no server key. The optional
	// vault only reads revisions saved by earlier encrypted deployments.
	vault, _ := sd.NewVault(cfg.MasterKey)
	accounts := sd.NewAccountService(r, vault, func(ctx context.Context, token, account, worker string) (sd.ConnectionCheck, error) {
		p, e := cf.NewClient(token, cf.Options{}).Probe(ctx, account, worker)
		return sd.ConnectionCheck{Readable: p.Readable, ListReadable: p.ListReadable, Subdomain: p.Subdomain}, e
	})
	verifier := sd.NewOriginVerifier()
	targets := sd.NewTargetService(r, accounts, installation, func(token, marker string) sd.Remote {
		return &sd.CloudflareRemote{Client: cf.NewClient(token, cf.Options{}), Marker: marker}
	}, func(ctx context.Context, token string, t sd.Target) error {
		return cf.NewClient(token, cf.Options{}).EnableSubdomain(ctx, t.AccountID, t.WorkerName)
	}, verifier.Check)
	return &handlers.StaticDeliveryAdmin{Accounts: accounts, Targets: targets}
}
