package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/services"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestTemplateImpactIncludesIndirectAndBodyReferencesWithoutPublishing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h := &Handler{db: db, snippetService: services.NewSnippetService(db)}
	name := "impact-" + primitive.NewObjectID().Hex()
	outer := name + "-outer"
	if _, err := h.snippetService.CreateSnippet(ctx, outer, "[[include:"+name+"]]"); err != nil {
		t.Fatal(err)
	}
	tid := primitive.NewObjectID()
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{"_id": tid, "html_layout": "<head>[[include:" + outer + "]]</head>"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []bson.M{
		{"_id": primitive.NewObjectID(), "template_id": tid, "title": "Layout", "published": true},
		{"_id": primitive.NewObjectID(), "template_id": primitive.NewObjectID(), "title": "Body", "data": bson.M{"body": "[[include:" + name + "]]"}},
		{"_id": primitive.NewObjectID(), "template_id": primitive.NewObjectID(), "title": "Mention", "data": bson.M{"body": name}},
		{"_id": primitive.NewObjectID(), "template_id": tid, "title": "Fork", "fork_id": primitive.NewObjectID()},
	} {
		if _, err := db.Collection("content").InsertOne(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	before, err := db.Collection("content_publications").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	got := h.currentTemplateImpact(ctx, primitive.NilObjectID, name)
	if got.Unavailable || got.Total != 2 || got.Published != 1 {
		t.Fatalf("impact=%+v", got)
	}
	after, err := db.Collection("content_publications").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("impact read published content")
	}
}
