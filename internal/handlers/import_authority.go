package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/services"
)

func importAuthorityContext(ctx context.Context, user *auth.SessionUser) context.Context {
	if user == nil {
		return ctx
	}
	return services.WithImportAuthority(ctx, models.ImportAuthority{UserID: user.ID, Owner: user.OperationOwner(), Role: user.Role, Scopes: user.Scopes, SandboxOnly: user.SandboxOnly})
}
