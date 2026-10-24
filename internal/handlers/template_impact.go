package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/dependencies"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type templateImpact struct {
	IndexedAt   time.Time
	Total       int
	Published   int
	Items       []impactArticle
	Truncated   bool
	Unavailable bool
}

type dependencyReader interface {
	Query(context.Context, primitive.ObjectID, string) (dependencies.Result, error)
	Wake()
}

func (h *Handler) EnableDependencyIndex(ctx context.Context) {
	index := dependencies.New(h.db)
	h.dependencyIndex = index
	go index.Run(ctx)
}

type impactArticle struct {
	ID         primitive.ObjectID `bson:"_id"`
	Title      string             `bson:"title"`
	FullPath   string             `bson:"full_path"`
	TemplateID primitive.ObjectID `bson:"template_id"`
	Published  bool               `bson:"published"`
	Data       map[string]any     `bson:"data"`
}

// This is a conservative inventory of current editable references, not a
// promise to rewrite frozen publications. Stream projections, not embeddings.
func (h *Handler) currentTemplateImpact(ctx context.Context, templateID primitive.ObjectID, snippet string) templateImpact {
	result := templateImpact{Items: []impactArticle{}}
	if h.dependencyIndex != nil {
		indexed, err := h.dependencyIndex.Query(ctx, templateID, snippet)
		if err != nil {
			result.Unavailable = true
			h.dependencyIndex.Wake()
			return result
		}
		result.Total = int(indexed.Total)
		result.Published = int(indexed.Published)
		result.IndexedAt = indexed.IndexedAt
		result.Truncated = indexed.Total > 50
		for _, row := range indexed.Items {
			result.Items = append(result.Items, impactArticle{ID: row.ID, Title: row.Title, FullPath: row.FullPath, TemplateID: row.TemplateID, Published: row.Published})
		}
		if time.Since(indexed.IndexedAt) > time.Minute {
			h.dependencyIndex.Wake()
		}
		return result
	}
	templates := map[primitive.ObjectID]bool{}
	names := map[string]bool{}
	if !templateID.IsZero() {
		templates[templateID] = true
	}
	if snippet != "" {
		names[snippet] = true
		snippets, err := h.snippetService.ListSnippets(ctx)
		if err != nil {
			result.Unavailable = true
			return result
		}
		for round := 0; round < 3; round++ {
			for _, s := range snippets {
				if referencesSnippet(s.HTML, names) {
					names[s.Name] = true
				}
			}
		}
		cur, err := h.db.Collection("templates").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"_id": 1, "html_layout": 1}).SetBatchSize(100))
		if err != nil {
			result.Unavailable = true
			return result
		}
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			var row struct {
				ID   primitive.ObjectID `bson:"_id"`
				HTML string             `bson:"html_layout"`
			}
			if cur.Decode(&row) != nil {
				result.Unavailable = true
				return result
			}
			if referencesSnippet(row.HTML, names) {
				templates[row.ID] = true
			}
		}
		if cur.Err() != nil {
			result.Unavailable = true
			return result
		}
	}
	filter := bson.M{"deleted": bson.M{"$ne": true}, "fork_id": nil}
	if snippet == "" {
		filter["template_id"] = templateID
	}
	cur, err := h.db.Collection("content").Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1, "title": 1, "full_path": 1, "template_id": 1, "published": 1, "data": 1}).SetSort(bson.D{{Key: "_id", Value: 1}}).SetBatchSize(100))
	if err != nil {
		result.Unavailable = true
		return result
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var article impactArticle
		if cur.Decode(&article) != nil {
			result.Unavailable = true
			return result
		}
		affected := templates[article.TemplateID]
		if !affected && snippet != "" {
			for _, value := range article.Data {
				if text, ok := value.(string); ok && referencesSnippet(text, names) {
					affected = true
					break
				}
			}
		}
		if !affected {
			continue
		}
		result.Total++
		if article.Published {
			result.Published++
		}
		article.Data = nil
		if len(result.Items) < 50 {
			result.Items = append(result.Items, article)
		}
	}
	result.Truncated = result.Total > len(result.Items)
	result.Unavailable = cur.Err() != nil
	return result
}

func referencesSnippet(text string, names map[string]bool) bool {
	for name := range names {
		if strings.Contains(text, "[[include:"+name+"]]") {
			return true
		}
		// Query card snippets are dependencies too, but a bare mention is not.
		if strings.Contains(text, "lc:query") && regexp.MustCompile(`\bsnippet\s*=\s*["']`+regexp.QuoteMeta(name)+`["']`).MatchString(text) {
			return true
		}
	}
	return false
}

const impactPanel = `
{{with .Impact}}<section class="form-card" aria-label="修改影响范围">
<h2>修改影响范围</h2>
{{if not .IndexedAt.IsZero}}<p>引用索引时间：{{.IndexedAt}}。近期编辑可能尚未计入，后台会刷新；该索引不用于决定发布内容。</p>{{end}}
<p>保存模板或片段不会自动覆盖已发布页面。请先保存并预览，再对选定文章明确执行重新发布；Cloudflare 副本需要另外预览、确认发布。</p>
{{if .Unavailable}}<p role="alert">暂时无法完整读取引用范围，请稍后刷新。此处不会执行发布。</p>{{else}}
<p>当前编辑内容中有 {{.Total}} 篇文章引用，其中 {{.Published}} 篇标记为已发布。列表是可能受影响的引用，不表示历史快照已经变更。</p>
<ul>{{range .Items}}<li><a href="/cm/content/{{.ID.Hex}}/edit">{{.Title}}</a> — {{.FullPath}}</li>{{end}}</ul>
{{if .Truncated}}<p>此处展示前50篇；完整内容请到内容管理筛选后逐项确认。</p>{{end}}{{end}}
</section>{{end}}`
