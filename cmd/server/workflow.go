package main

import (
	"context"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/generation"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"strings"
)

func authorizeWorkflowImport(ctx context.Context, db *database.DB, j jobs.Job) error {
	id, err := primitive.ObjectIDFromHex(j.InputRef)
	if err != nil {
		return err
	}
	var record models.ImportJob
	if err := db.FindOne(ctx, "import_jobs", bson.M{"_id": id}, &record); err != nil {
		return err
	}
	if j.Actor == "scheduler" {
		if record.SourceID == nil {
			return fmt.Errorf("scheduled source missing")
		}
		var source models.ImportSource
		if err := db.FindOne(ctx, "import_sources", bson.M{"_id": *record.SourceID, "active": true}, &source); err != nil {
			return err
		}
		if record.RequiresPublish {
			var owner models.User
			if source.CreatedBy.IsZero() {
				return fmt.Errorf("scheduled publication owner unavailable")
			}
			if err := db.FindOne(ctx, "users", bson.M{"_id": source.CreatedBy, "disabled": bson.M{"$ne": true}}, &owner); err != nil {
				return err
			}
			if !auth.HasPermission(owner.Role, auth.PermContentPublish) {
				return fmt.Errorf("scheduled publication permission revoked")
			}
		}
		return nil
	}
	if record.Authority == nil || record.Authority.SandboxOnly {
		return fmt.Errorf("import authority snapshot unavailable or sandbox restricted")
	}
	a := record.Authority
	uid, err := primitive.ObjectIDFromHex(a.UserID)
	if err != nil {
		return err
	}
	var user models.User
	if err := db.FindOne(ctx, "users", bson.M{"_id": uid, "disabled": bson.M{"$ne": true}}, &user); err != nil {
		return err
	}
	frozen := &auth.SessionUser{ID: a.UserID, Role: a.Role, Scopes: a.Scopes}
	if a.Role == "admin" && user.Role != "admin" {
		return fmt.Errorf("import administrative authority revoked")
	}
	current := &auth.SessionUser{ID: a.UserID, Role: user.Role, Scopes: a.Scopes}
	if strings.HasPrefix(a.Owner, "apikey:") {
		kid, err := primitive.ObjectIDFromHex(strings.TrimPrefix(a.Owner, "apikey:"))
		if err != nil {
			return err
		}
		var key models.APIKey
		if err := db.FindOne(ctx, "api_keys", bson.M{"_id": kid, "user_id": uid}, &key); err != nil {
			return err
		}
		if key.SandboxOnly {
			return fmt.Errorf("import credential is sandbox restricted")
		}
		current.Scopes = key.Scopes
	} else if a.Owner != a.UserID {
		return fmt.Errorf("unsupported import credential owner")
	}
	permissions := []string{auth.PermContentEdit}
	if record.RequiresPublish {
		permissions = append(permissions, auth.PermContentPublish)
	}
	for _, permission := range permissions {
		if !auth.UserHasPermission(frozen, permission) || !auth.UserHasPermission(current, permission) {
			return fmt.Errorf("import permission revoked: %s", permission)
		}
	}
	return nil
}

func authorizeWorkflowUpgrade(ctx context.Context, db *database.DB, actor generation.Actor) error {
	if !actor.Authenticated || !actor.Can(generation.ScopeContentEdit) || !actor.Can(generation.ScopeContentPublish) {
		return fmt.Errorf("upgrade scope revoked")
	}
	id, err := primitive.ObjectIDFromHex(actor.ID)
	if err != nil {
		return err
	}
	var user models.User
	if err := db.FindOne(ctx, "users", bson.M{"_id": id, "disabled": bson.M{"$ne": true}}, &user); err != nil {
		return err
	}
	current := actor
	current.Role = user.Role
	current.IsAdmin = user.Role == models.RoleAdmin
	if actor.IsAdmin && !current.IsAdmin {
		return fmt.Errorf("upgrade administrative authority revoked")
	}
	if strings.HasPrefix(actor.CredentialOwner, "apikey:") {
		keyID, err := primitive.ObjectIDFromHex(strings.TrimPrefix(actor.CredentialOwner, "apikey:"))
		if err != nil {
			return err
		}
		var key models.APIKey
		if err := db.FindOne(ctx, "api_keys", bson.M{"_id": keyID, "user_id": id}, &key); err != nil {
			return err
		}
		current.Scopes = key.Scopes
		current.SandboxOnly = key.SandboxOnly
	}
	if strings.HasPrefix(actor.CredentialOwner, "oauth:") {
		parts := strings.Split(actor.CredentialOwner, ":")
		if len(parts) != 3 {
			return fmt.Errorf("invalid oauth attribution")
		}
		var client models.OAuthClient
		if err := db.FindOne(ctx, "oauth_clients", bson.M{"client_id": parts[1]}, &client); err != nil {
			return err
		}
	}
	if !current.Can(generation.ScopeContentEdit) || !current.Can(generation.ScopeContentPublish) {
		return fmt.Errorf("upgrade permission revoked")
	}
	return nil
}
