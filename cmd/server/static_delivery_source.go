package main

import (
	"context"

	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"github.com/metavert-io/lightcms/v7/internal/product/templatecontract"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// This adapter is read-only. It never reads mutable title/body/template data
// as a substitute for a frozen publication or writes lifecycle/retention state.
type staticDeliverySourceLookup struct{ db *database.DB }

func (s staticDeliverySourceLookup) Content(ctx context.Context, id primitive.ObjectID) (staticexport.ContentState, error) {
	var c models.Content
	err := s.db.Content().FindOne(ctx, bson.M{"_id": id}).Decode(&c)
	return staticexport.ContentState{Deleted: c.Deleted || c.DeletedAt != nil, Fork: c.ForkID != nil}, err
}
func (s staticDeliverySourceLookup) TemplateSlug(ctx context.Context, id primitive.ObjectID) (string, error) {
	v, err := templatecontract.NewRepository(s.db).FindVersionByID(ctx, id)
	return v.Slug, err
}
