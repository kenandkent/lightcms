// Package dependencies maintains advisory editable-content references. It is
// never used to render or rewrite immutable Publications or Worker releases.
package dependencies

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var include = regexp.MustCompile(`\[\[include:([^\]\s]+)\]\]`)
var query = regexp.MustCompile(`(?is)<lc:query\b[^>]*>`)
var snippetAttribute = regexp.MustCompile(`\bsnippet\s*=\s*["']([^"']+)["']`)

func references(text string) []string {
	names := map[string]bool{}
	for _, m := range include.FindAllStringSubmatch(text, -1) {
		names[m[1]] = true
	}
	for _, tag := range query.FindAllString(text, -1) {
		for _, m := range snippetAttribute.FindAllStringSubmatch(tag, -1) {
			names[m[1]] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
func closure(start []string, graph map[string][]string) []string {
	seen := map[string]bool{}
	pending := append([]string(nil), start...)
	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		pending = append(pending, graph[name]...)
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

type Article struct {
	ID         primitive.ObjectID `bson:"content_id"`
	Title      string             `bson:"title"`
	FullPath   string             `bson:"full_path"`
	TemplateID primitive.ObjectID `bson:"template_id"`
	Published  bool               `bson:"published"`
}
type Result struct {
	Total, Published int64
	Items            []Article
	IndexedAt        time.Time
}
type Index struct {
	db   *database.DB
	mu   sync.Mutex
	wake chan struct{}
}

func New(db *database.DB) *Index { return &Index{db: db, wake: make(chan struct{}, 1)} }
func (i *Index) Wake() {
	select {
	case i.wake <- struct{}{}:
	default:
	}
}
func (i *Index) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 4*time.Minute)
		err := i.Rebuild(work)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("dependency index refresh: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-i.wake:
		}
		// Coalesce edit/read bursts; one rebuild at most every 30 seconds.
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (i *Index) Rebuild(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, err := i.db.Collection("dependency_rows").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "generation", Value: 1}, {Key: "template_id", Value: 1}, {Key: "content_id", Value: 1}}},
		{Keys: bson.D{{Key: "generation", Value: 1}, {Key: "snippets", Value: 1}, {Key: "content_id", Value: 1}}},
		{Keys: bson.D{{Key: "generation", Value: 1}, {Key: "assets", Value: 1}, {Key: "content_id", Value: 1}}},
	}); err != nil {
		return err
	}
	generation := primitive.NewObjectID()
	started := time.Now()
	graph := map[string][]string{}
	templates := map[primitive.ObjectID][]string{}
	known, err := i.knownAssets(ctx)
	if err != nil {
		return err
	}
	snippetAssets := map[string][]string{}
	templateAssets := map[primitive.ObjectID][]string{}
	budget := 0
	for _, collection := range []string{"snippets", "templates"} {
		cur, err := i.db.Collection(collection).Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1, "html": 1, "html_layout": 1}).SetBatchSize(50))
		if err != nil {
			return err
		}
		err = func() error {
			defer cur.Close(ctx)
			for cur.Next(ctx) {
				var row struct {
					ID     primitive.ObjectID `bson:"_id"`
					Name   string             `bson:"name"`
					HTML   string             `bson:"html"`
					Layout string             `bson:"html_layout"`
				}
				if err := cur.Decode(&row); err != nil {
					return err
				}
				budget += len(row.HTML) + len(row.Layout)
				if budget > 32<<20 || len(graph)+len(templates) > 10000 {
					return fmt.Errorf("dependency graph exceeds bounded build budget")
				}
				if collection == "snippets" {
					graph[row.Name] = references(row.HTML)
					snippetAssets[row.Name] = assetReferences(row.HTML, known)
				} else {
					templates[row.ID] = references(row.Layout)
					templateAssets[row.ID] = assetReferences(row.Layout, known)
				}
			}
			return cur.Err()
		}()
		if err != nil {
			return err
		}
	}
	for id, refs := range templates {
		templates[id] = closure(refs, graph)
	}
	cur, err := i.db.Collection("content").Find(ctx, bson.M{"deleted": bson.M{"$ne": true}, "fork_id": nil}, options.Find().SetProjection(bson.M{"title": 1, "full_path": 1, "template_id": 1, "published": 1, "data": 1}).SetBatchSize(25))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row struct {
			ID         primitive.ObjectID `bson:"_id"`
			Title      string             `bson:"title"`
			FullPath   string             `bson:"full_path"`
			TemplateID primitive.ObjectID `bson:"template_id"`
			Published  bool               `bson:"published"`
			Data       map[string]any     `bson:"data"`
		}
		if err := cur.Decode(&row); err != nil {
			return err
		}
		refs := append([]string(nil), templates[row.TemplateID]...)
		for _, v := range row.Data {
			if text, ok := v.(string); ok {
				refs = append(refs, references(text)...)
			}
		}
		refs = closure(refs, graph)
		assets := append([]string(nil), templateAssets[row.TemplateID]...)
		assets = append(assets, dataAssets(row.Data, known, 0)...)
		for _, name := range refs {
			assets = append(assets, snippetAssets[name]...)
		}
		assets = closure(assets, nil)
		if len(refs) > 10000 {
			return fmt.Errorf("article dependencies exceed index budget")
		}
		_, err := i.db.Collection("dependency_rows").InsertOne(ctx, bson.M{"_id": generation.Hex() + ":" + row.ID.Hex(), "generation": generation, "content_id": row.ID, "title": row.Title, "full_path": row.FullPath, "template_id": row.TemplateID, "published": row.Published, "snippets": refs, "assets": assets, "created_at": started})
		if err != nil {
			return err
		}
	}
	if err := cur.Err(); err != nil {
		return err
	}
	// Publish only a complete generation. A failed refresh leaves the previous
	// generation readable and its timestamp visibly old.
	if err := i.rebuildFrozen(ctx, generation, started); err != nil {
		return err
	}
	_, err = i.db.Collection("dependency_state").UpdateOne(ctx, bson.M{"_id": "current"}, bson.M{"$set": bson.M{"generation": generation, "indexed_at": started}}, options.Update().SetUpsert(true))
	if err != nil {
		return err
	}
	// Retain previous generations for in-flight readers, never delete source data.
	_, err = i.db.Collection("dependency_publications").DeleteMany(ctx, bson.M{"generation": bson.M{"$ne": generation}, "indexed_at": bson.M{"$lt": time.Now().Add(-24 * time.Hour)}})
	if err != nil {
		return err
	}
	_, err = i.db.Collection("dependency_rows").DeleteMany(ctx, bson.M{"generation": bson.M{"$ne": generation}, "created_at": bson.M{"$lt": time.Now().Add(-24 * time.Hour)}})
	return err
}
func (i *Index) Query(ctx context.Context, templateID primitive.ObjectID, snippet string) (Result, error) {
	filter := bson.M{"template_id": templateID}
	if snippet != "" {
		filter = bson.M{"snippets": snippet}
	}
	return i.query(ctx, filter)
}
func (i *Index) query(ctx context.Context, filter bson.M) (Result, error) {
	result := Result{Items: []Article{}}
	var state struct {
		Generation primitive.ObjectID `bson:"generation"`
		At         time.Time          `bson:"indexed_at"`
	}
	if err := i.db.FindOne(ctx, "dependency_state", bson.M{"_id": "current"}, &state); err != nil {
		return result, err
	}
	result.IndexedAt = state.At
	filter["generation"] = state.Generation
	n, err := i.db.Collection("dependency_rows").CountDocuments(ctx, filter)
	if err != nil {
		return result, err
	}
	result.Total = n
	filter["published"] = true
	n, err = i.db.Collection("dependency_rows").CountDocuments(ctx, filter)
	if err != nil {
		return result, err
	}
	result.Published = n
	delete(filter, "published")
	cur, err := i.db.Collection("dependency_rows").Find(ctx, filter, options.Find().SetProjection(bson.M{"content_id": 1, "title": 1, "full_path": 1, "template_id": 1, "published": 1}).SetSort(bson.D{{Key: "content_id", Value: 1}}).SetLimit(50))
	if err != nil {
		return result, err
	}
	defer cur.Close(ctx)
	err = cur.All(ctx, &result.Items)
	return result, err
}
