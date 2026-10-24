package main

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/generation"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestImportWorkflowRejectsRevokedCredential(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	uid, kid, jid := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "users", models.User{ID: uid, Email: "import-owner@example.com", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "api_keys", models.APIKey{ID: kid, UserID: &uid}); err != nil {
		t.Fatal(err)
	}
	record := models.ImportJob{ID: jid, CreatedBy: "import-owner@example.com", RequiresPublish: true, Authority: &models.ImportAuthority{UserID: uid.Hex(), Owner: "apikey:" + kid.Hex(), Role: "admin"}}
	if _, err := db.InsertOne(ctx, "import_jobs", record); err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{Actor: record.CreatedBy, InputRef: jid.Hex()}
	if err := authorizeWorkflowImport(ctx, db, job); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("api_keys").DeleteOne(ctx, bson.M{"_id": kid}); err != nil {
		t.Fatal(err)
	}
	if err := authorizeWorkflowImport(ctx, db, job); err == nil {
		t.Fatal("revoked API credential can still execute frozen import")
	}
}

func TestUpgradeWorkflowRejectsAdministrativePolicyAfterRoleDowngrade(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	uid := primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "users", models.User{ID: uid, Email: "upgrade@example.com", Role: models.RoleEditor}); err != nil {
		t.Fatal(err)
	}
	actor := generation.Actor{ID: uid.Hex(), Role: models.RoleAdmin, IsAdmin: true, Authenticated: true}
	if err := authorizeWorkflowUpgrade(ctx, db, actor); err == nil {
		t.Fatal("frozen administrator policy survived role downgrade")
	}
}
