package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"log"
	"net/url"
	"time"
)

// RefreshPageStatus is an advisory read model, not publication authority. Its
// rows are replaced only while the target still points at this verified release.
func (r *Repository) RefreshPageStatus(ctx context.Context, release primitive.ObjectID) error {
	if r.summaries == nil {
		return nil
	}
	manifest, err := r.summaries.Summary(ctx, release)
	if err != nil {
		return err
	}
	if _, err := r.collection("page_status").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "content_id", Value: 1}, {Key: "target_id", Value: 1}}}); err != nil {
		return err
	}
	return r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var target Target
		if err := r.collection("targets").FindOne(sc, bson.M{"_id": manifest.TargetID, "current_release_id": release}).Decode(&target); err == mongo.ErrNoDocuments {
			return nil
		} else if err != nil {
			return err
		}
		// Shared target write fence serializes against a new verified release commit.
		if _, err := r.collection("targets").UpdateOne(sc, bson.M{"_id": target.ID, "current_release_id": release}, bson.M{"$set": bson.M{"page_status_revision": target.CurrentRevision}}); err != nil {
			return err
		}
		if _, err := r.collection("page_status").DeleteMany(sc, bson.M{"target_id": target.ID}); err != nil {
			return err
		}
		rows := make([]any, 0, len(manifest.Pages))
		for path, page := range manifest.Pages {
			rows = append(rows, bson.M{"_id": target.ID.Hex() + ":" + releaseHash([]byte(path)), "target_id": target.ID, "target_name": target.Name, "release_id": release, "revision": target.CurrentRevision, "content_id": page.ContentID, "publication_id": page.PublicationID, "path": path, "url": manifest.Origin + (&url.URL{Path: path}).EscapedPath(), "indexed_at": time.Now().UTC(), "verified_release": true})
		}
		if len(rows) == 0 {
			return nil
		}
		_, err := r.collection("page_status").InsertMany(sc, rows)
		return err
	})
}
func (s *CommandService) RebuildPageStatus(ctx context.Context) error {
	cur, err := s.repo.collection("targets").Find(ctx, bson.M{"current_release_id": bson.M{"$nin": bson.A{nil, primitive.NilObjectID}}}, options.Find().SetProjection(bson.M{"current_release_id": 1}).SetBatchSize(50))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var target Target
		if err := cur.Decode(&target); err != nil {
			return err
		}
		if err := s.repo.RefreshPageStatus(ctx, target.CurrentReleaseID); err != nil {
			log.Printf("delivery page status target=%s unavailable: %v", target.ID.Hex(), err)
		}
	}
	return cur.Err()
}
