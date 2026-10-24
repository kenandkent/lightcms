package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type contentVersionState struct {
	Draft, Published int64
	Changed          bool
	Workers          []workerVersion
}
type workerVersion struct {
	ContentID     primitive.ObjectID `bson:"content_id"`
	PublicationID primitive.ObjectID `bson:"publication_id"`
	Name          string             `bson:"target_name"`
	URL           string             `bson:"url"`
	Version       int64
}

func (h *Handler) contentVersionStates(ctx context.Context, content []models.Content) (map[string]contentVersionState, error) {
	result := map[string]contentVersionState{}
	ids := []primitive.ObjectID{}
	for _, c := range content {
		ids = append(ids, c.ID)
		result[c.ID.Hex()] = contentVersionState{Draft: c.CurrentVersion}
	}
	if len(ids) == 0 {
		return result, nil
	}
	cursor, err := h.db.Collection("content_publications").Find(ctx, bson.M{"content_id": bson.M{"$in": ids}, "status": "active"}, options.Find().SetProjection(bson.M{"content_id": 1, "content_version": 1}))
	if err != nil {
		return result, err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var p struct {
			ContentID primitive.ObjectID `bson:"content_id"`
			Version   int64              `bson:"content_version"`
		}
		if err := cursor.Decode(&p); err != nil {
			return result, err
		}
		state := result[p.ContentID.Hex()]
		state.Published = p.Version
		state.Changed = state.Draft > 0 && p.Version > 0 && state.Draft != p.Version
		result[p.ContentID.Hex()] = state
	}
	if err := cursor.Err(); err != nil {
		return result, err
	}
	// Join current target pointers: an asynchronously refreshed index can omit a
	// new entry temporarily, but cannot present an old release as current.
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"content_id": bson.M{"$in": ids}}}},
		{{Key: "$lookup", Value: bson.M{"from": "static_delivery_targets", "localField": "target_id", "foreignField": "_id", "as": "target"}}},
		{{Key: "$unwind", Value: "$target"}},
		{{Key: "$match", Value: bson.M{"$expr": bson.M{"$eq": bson.A{"$release_id", "$target.current_release_id"}}}}},
		{{Key: "$project", Value: bson.M{"content_id": 1, "publication_id": 1, "target_name": 1, "url": 1}}},
		{{Key: "$limit", Value: 500}},
	}
	workers, err := h.db.Collection("static_delivery_page_status").Aggregate(ctx, pipeline)
	if err != nil {
		return result, err
	}
	defer workers.Close(ctx)
	var rows []workerVersion
	if err := workers.All(ctx, &rows); err != nil {
		return result, err
	}
	publications := []primitive.ObjectID{}
	for _, row := range rows {
		publications = append(publications, row.PublicationID)
	}
	versions := map[primitive.ObjectID]int64{}
	if len(publications) > 0 {
		cur, err := h.db.Collection("content_publications").Find(ctx, bson.M{"_id": bson.M{"$in": publications}}, options.Find().SetProjection(bson.M{"content_version": 1}))
		if err != nil {
			return result, err
		}
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			var row struct {
				ID      primitive.ObjectID `bson:"_id"`
				Version int64              `bson:"content_version"`
			}
			if err := cur.Decode(&row); err != nil {
				return result, err
			}
			versions[row.ID] = row.Version
		}
		if err := cur.Err(); err != nil {
			return result, err
		}
	}
	for _, row := range rows {
		row.Version = versions[row.PublicationID]
		state := result[row.ContentID.Hex()]
		state.Workers = append(state.Workers, row)
		result[row.ContentID.Hex()] = state
	}
	return result, nil
}
