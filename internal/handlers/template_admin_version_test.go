package handlers

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAdminTemplateUpdatePreservesSlugAndVersionsLayout(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	id := seedTemplate(t, h.db, "中文模板", "stable-template")
	form := url.Values{"name": {"中文模板"}, "html_layout": {`<head>[[include:test]]</head>`}}
	originalField := models.TemplateField{Name: "image", Label: "Image", Type: "url", Default: "https://example.com/image.png", Description: "Preview", Example: "https://example.com/example.png", Validation: models.FieldValidation{AllowedProtocols: []string{"https"}}}
	if err := h.db.UpdateOne(context.Background(), "templates", bson.M{"_id": id}, bson.M{"$set": bson.M{"fields": []models.TemplateField{originalField}}}); err != nil {
		t.Fatal(err)
	}
	form["field_name[]"] = []string{"image"}
	form["field_label[]"] = []string{"Image"}
	form["field_type[]"] = []string{"url"}
	rr := rbacPost(t, h.UpdateTemplate, form, map[string]string{"id": id.Hex()}, "admin")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	var saved models.Template
	if err := h.db.FindOne(context.Background(), "templates", bson.M{"_id": id}, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Slug != "stable-template" || saved.CurrentVersion != 2 || saved.HTMLLayout != form.Get("html_layout") {
		t.Fatalf("ordinary edit must preserve slug and create v2: slug=%q version=%d layout=%q", saved.Slug, saved.CurrentVersion, saved.HTMLLayout)
	}
	if len(saved.Fields) != 1 || saved.Fields[0].Default != originalField.Default || saved.Fields[0].Description != originalField.Description || saved.Fields[0].Example != originalField.Example || len(saved.Fields[0].Validation.AllowedProtocols) != 1 || saved.Fields[0].Validation.AllowedProtocols[0] != "https" {
		t.Fatalf("layout edit lost field metadata not exposed by the form: %+v", saved.Fields)
	}
	var version struct {
		HTMLLayout string `bson:"html_layout"`
	}
	if err := h.db.FindOne(context.Background(), "template_versions", bson.M{"template_id": id, "version": int64(2)}, &version); err != nil {
		t.Fatal(err)
	}
	if version.HTMLLayout != saved.HTMLLayout {
		t.Fatal("mutable layout differs from immutable version")
	}
}

func TestAdminTemplateDeletePreservesReferencedTemplate(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	id := seedTemplate(t, h.db, "Used", "used-template")
	ctx := context.Background()
	if _, err := h.db.Collection("content").InsertOne(ctx, bson.M{"_id": primitive.NewObjectID(), "template_id": id, "full_path": "/used", "published": true}); err != nil {
		t.Fatal(err)
	}
	rr := rbacPost(t, h.DeleteTemplate, url.Values{}, map[string]string{"id": id.Hex()}, "admin")
	if rr.Code != http.StatusConflict {
		t.Fatalf("referenced template deletion: %d", rr.Code)
	}
	if n, err := h.db.Collection("templates").CountDocuments(ctx, bson.M{"_id": id}); err != nil || n != 1 {
		t.Fatalf("template lost: %d %v", n, err)
	}
}
