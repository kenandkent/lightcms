package dependencies

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/net/html"
)

var cssURL = regexp.MustCompile(`(?i)url\(\s*['"]?([^\s'"\)]+)`)

// Resource references are advisory: an absolute external URL with the same
// path can conservatively match a local asset. Never use this to authorize GC.
func assetReferences(text string, known map[string]bool) []string {
	seen := map[string]bool{}
	add := func(value string) {
		u, err := url.Parse(strings.TrimSpace(value))
		if err == nil && known[u.Path] {
			seen[u.Path] = true
		}
	}
	add(text)
	z := html.NewTokenizer(strings.NewReader(text))
	for {
		token := z.Next()
		if token == html.ErrorToken {
			break
		}
		if token != html.StartTagToken && token != html.SelfClosingTagToken {
			continue
		}
		t := z.Token()
		for _, a := range t.Attr {
			switch a.Key {
			case "src", "href", "poster":
				add(a.Val)
			case "srcset":
				for _, entry := range strings.Split(a.Val, ",") {
					parts := strings.Fields(entry)
					if len(parts) > 0 {
						add(parts[0])
					}
				}
			}
		}
	}
	for _, m := range cssURL.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func (i *Index) knownAssets(ctx context.Context) (map[string]bool, error) {
	cur, err := i.db.Collection("assets").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"serve_path": 1, "full_path": 1}).SetBatchSize(100))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	result := map[string]bool{}
	bytes := 0
	for cur.Next(ctx) {
		var row struct {
			Serve string `bson:"serve_path"`
			Path  string `bson:"full_path"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		for _, path := range []string{row.Serve, row.Path} {
			if path != "" {
				result[path] = true
				bytes += len(path)
			}
		}
		if len(result) > 10000 || bytes > 4<<20 {
			return nil, fmt.Errorf("asset dependency catalog exceeds budget")
		}
	}
	return result, cur.Err()
}
func dataAssets(value any, known map[string]bool, depth int) []string {
	if depth > 16 {
		return nil
	}
	var result []string
	switch v := value.(type) {
	case string:
		result = assetReferences(v, known)
	case map[string]any:
		for _, item := range v {
			result = append(result, dataAssets(item, known, depth+1)...)
		}
	case bson.M:
		for _, item := range v {
			result = append(result, dataAssets(item, known, depth+1)...)
		}
	case bson.A:
		for _, item := range v {
			result = append(result, dataAssets(item, known, depth+1)...)
		}
	case []any:
		for _, item := range v {
			result = append(result, dataAssets(item, known, depth+1)...)
		}
	}
	return result
}

// Copy only provenance already frozen by Publication. Hash-only legacy
// dependencies remain hash-only; never expand them using today's template.
func (i *Index) rebuildFrozen(ctx context.Context, generation primitive.ObjectID, at time.Time) error {
	if _, err := i.db.Collection("dependency_publications").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "generation", Value: 1}, {Key: "content_id", Value: 1}, {Key: "publication_id", Value: 1}}}); err != nil {
		return err
	}
	cur, err := i.db.Collection("content_publications").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"content_id": 1, "template_id": 1, "template_version_id": 1, "content_version": 1, "status": 1, "dependency_snapshot": 1, "render_dependencies_hash": 1}).SetBatchSize(25))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row bson.M
		if err := cur.Decode(&row); err != nil {
			return err
		}
		raw, err := bson.Marshal(row)
		if err != nil {
			return err
		}
		if len(raw) > 256<<10 {
			return fmt.Errorf("frozen dependency metadata exceeds budget")
		}
		id, ok := row["_id"].(primitive.ObjectID)
		if !ok {
			return fmt.Errorf("invalid publication identity")
		}
		row["_id"] = generation.Hex() + ":" + id.Hex()
		row["publication_id"] = id
		row["generation"] = generation
		row["indexed_at"] = at
		if _, err := i.db.Collection("dependency_publications").InsertOne(ctx, row); err != nil {
			return err
		}
	}
	return cur.Err()
}
func (i *Index) QueryAsset(ctx context.Context, path string) (Result, error) {
	return i.query(ctx, bson.M{"assets": path})
}
func (i *Index) Frozen(ctx context.Context, content primitive.ObjectID) ([]bson.M, error) {
	var state struct {
		Generation primitive.ObjectID `bson:"generation"`
	}
	if err := i.db.FindOne(ctx, "dependency_state", bson.M{"_id": "current"}, &state); err != nil {
		return nil, err
	}
	cur, err := i.db.Collection("dependency_publications").Find(ctx, bson.M{"generation": state.Generation, "content_id": content}, options.Find().SetProjection(bson.M{"_id": 0, "generation": 0}).SetSort(bson.D{{Key: "publication_id", Value: -1}}).SetLimit(50))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []bson.M
	err = cur.All(ctx, &rows)
	return rows, err
}
