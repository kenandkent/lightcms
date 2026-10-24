// Package contentaudit inventories legacy history gaps without inventing or
// repairing old versions. Run explicitly; it is not an authoring dependency.
package contentaudit

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Range struct{ From, To int64 }
type Issue struct {
	ContentID primitive.ObjectID `json:"content_id"`
	Path      string             `json:"path"`
	Current   int64              `json:"current_version"`
	Missing   []Range            `json:"missing_ranges,omitempty"`
	Code      string             `json:"code"`
}
type Page struct {
	Scanned int                `json:"scanned"`
	Next    primitive.ObjectID `json:"next,omitempty"`
	Issues  []Issue            `json:"issues"`
}

func Scan(ctx context.Context, db *database.DB, after primitive.ObjectID) (Page, error) {
	result := Page{Issues: []Issue{}}
	cur, err := db.Collection("content").Find(ctx, bson.M{"_id": bson.M{"$gt": after}}, options.Find().SetProjection(bson.M{"full_path": 1, "current_version": 1}).SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(101))
	if err != nil {
		return result, err
	}
	defer cur.Close(ctx)
	var last primitive.ObjectID
	for cur.Next(ctx) {
		if result.Scanned == 100 {
			result.Next = last
			break
		}
		var row struct {
			ID      primitive.ObjectID `bson:"_id"`
			Path    string             `bson:"full_path"`
			Version int64              `bson:"current_version"`
		}
		if err := cur.Decode(&row); err != nil {
			return result, err
		}
		result.Scanned++
		last = row.ID
		issue := Issue{ContentID: row.ID, Path: row.Path, Current: row.Version}
		if row.Version < 1 {
			issue.Code = "LEGACY_VERSION_UNKNOWN"
			result.Issues = append(result.Issues, issue)
			continue
		}
		history, err := db.Collection("content_versions").Find(ctx, bson.M{"content_id": row.ID, "version": bson.M{"$gte": 1, "$lte": row.Version}}, options.Find().SetProjection(bson.M{"version": 1}).SetSort(bson.D{{Key: "version", Value: 1}}).SetLimit(10001))
		if err != nil {
			return result, err
		}
		expected, count := int64(1), 0
		for history.Next(ctx) {
			var v struct {
				Version int64 `bson:"version"`
			}
			if err := history.Decode(&v); err != nil {
				history.Close(ctx)
				return result, err
			}
			count++
			if count > 10000 {
				issue.Code = "HISTORY_SCAN_LIMIT"
				break
			}
			if v.Version < expected {
				issue.Code = "DUPLICATE_VERSION"
				continue
			}
			if v.Version > expected {
				issue.Missing = append(issue.Missing, Range{expected, v.Version - 1})
			}
			expected = v.Version + 1
			if expected < 1 {
				issue.Code = "INVALID_VERSION_RANGE"
				break
			}
		}
		err = history.Err()
		history.Close(ctx)
		if err != nil {
			return result, err
		}
		if count <= 10000 && expected > 0 && expected <= row.Version {
			issue.Missing = append(issue.Missing, Range{expected, row.Version})
		}
		if len(issue.Missing) > 0 && issue.Code == "" {
			issue.Code = "HISTORY_GAP"
		}
		if issue.Code != "" {
			result.Issues = append(result.Issues, issue)
		}
	}
	return result, cur.Err()
}
