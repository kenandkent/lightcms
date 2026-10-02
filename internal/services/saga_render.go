package services

// Production snapshot renderer for the V3 publication saga (R02).
//
// The saga's minimal DefaultRenderer marks every string trusted HTML with
// no sanitization, no Markdown conversion, and no snippet/wikilink/TOC
// handling — hostile text like `<img onerror>` ships verbatim into
// canonical bytes. SagaSnapshotRender instead builds a frozen
// publication.RenderSnapshot with the SAME inputs as the legacy
// ContentService render path (site script policy, snippet bodies, wikilink
// index, lc:query expansions) and runs it through RenderDetailed, so
// production bytes match the declared field types and script policy.
//
// Frozen-time rule: the snapshot carries the saga-frozen logical time and
// public URL; re-rendering it yields byte-identical HTML (crash takeover
// safe). lc:query expansions and the wikilink index are read live at plan
// time and frozen into the snapshot — exactly what makes them deterministic
// for retries.
//
// The complete snapshot is durably frozen before rendering for keyed
// attempts. Takeover reads it without querying live dependencies. The JSON
// snapshot is limited to 8 MiB; oversize planning fails before file activation.

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// rankScriptPolicy orders policies by strictness for strictest-wins
// resolution (unknown values rank strictest so PlanSnapshot rejects them
// fail-closed instead of rendering raw).
func rankScriptPolicy(p string) int {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "none":
		return 2
	case "admin_only":
		return 1
	default:
		if strings.TrimSpace(p) != "" && strings.ToLower(strings.TrimSpace(p)) != "all" {
			return 3
		}
		return 0
	}
}

// SagaSnapshotRender implements publication.SnapshotRenderFunc.
func (s *ContentService) SagaSnapshotRender(ctx context.Context, in publication.RenderInput) (publication.RenderResult, error) {
	var zero publication.RenderResult
	op, err := idempotency.RenderSnapshotForPublication(ctx, s.db, in.PublicationID)
	if err != nil {
		return zero, err
	}
	if op != nil && len(op.RenderSnapshot) > 0 {
		var snap publication.RenderSnapshot
		if err = json.Unmarshal(op.RenderSnapshot, &snap); err != nil {
			return zero, err
		}
		return publication.RenderDetailed(ctx, snap)
	}

	// 1. Script policy — site config first (same source and default as the
	// legacy path), with the template's own policy able to narrow toward
	// strictness (strictest wins; unknown values fail closed in PlanSnapshot).
	policy := "all"
	if cfg, err := s.db.GetSiteConfig(ctx); err == nil && cfg != nil && cfg.MarkdownScriptPolicy != "" {
		policy = cfg.MarkdownScriptPolicy
	}
	if rankScriptPolicy(in.Template.ScriptPolicy) > rankScriptPolicy(policy) {
		policy = in.Template.ScriptPolicy
	}

	// 2. Snippet bodies (name → HTML) for [[include:name]] expansion.
	snippets := map[string]string{}
	if cursor, err := s.db.FindMany(ctx, "snippets", bson.M{},
		options.Find().SetProjection(bson.M{"name": 1, "html": 1})); err == nil {
		for cursor.Next(ctx) {
			var snip struct {
				Name string `bson:"name"`
				HTML string `bson:"html"`
			}
			if cursor.Decode(&snip) == nil && snip.Name != "" {
				snippets[snip.Name] = snip.HTML
			}
		}
		cursor.Close(ctx)
	}

	// 3. Wikilink index — same membership query as the legacy builder
	// (published + not deleted; fork copies are never published so they
	// are excluded by construction), projected to title + path only.
	titleToPath := map[string]string{}
	pathToTitle := map[string]string{}
	if cursor, err := s.db.FindMany(ctx, "content", bson.M{
		"published": true, "deleted": bson.M{"$ne": true},
	}, options.Find().SetProjection(bson.M{"title": 1, "full_path": 1})); err == nil {
		for cursor.Next(ctx) {
			var c struct {
				Title    string `bson:"title"`
				FullPath string `bson:"full_path"`
			}
			if cursor.Decode(&c) == nil && c.FullPath != "" {
				titleToPath[strings.ToLower(c.Title)] = c.FullPath
				pathToTitle[c.FullPath] = c.Title
			}
		}
		cursor.Close(ctx)
	}

	// 4. lc:query expansions, frozen per directive. Each directive is
	// expanded through the same executor as the legacy path; failures
	// degrade to the legacy error comment (warn-and-continue, like legacy)
	// rather than failing the whole render.
	lcCache := map[string]string{}
	for _, d := range publication.ExtractLCQueryDirectives(in.Template.HTMLLayout) {
		out, err := s.processQueryDirectives(ctx, d)
		if err != nil {
			log.Printf("Warning: lc:query snapshot expansion error: %v", err)
		}
		lcCache[strings.TrimSpace(d)] = out
	}

	snap, err := publication.PlanSnapshot(
		in.Content, in.ContentVersion, in.Template, in.PublicationID,
		in.LogicalPublishedAt, in.PublicURL, publication.PlanOptions{
			ScriptPolicy:       policy,
			AuthorIsAdmin:      in.AuthorIsAdmin,
			DependencySnapshot: publication.DefaultDependencySnapshot(policy),
			Snippets:           snippets,
			TitleToPath:        titleToPath,
			PathToTitle:        pathToTitle,
			LCQueryCache:       lcCache,
		})
	if err != nil {
		return zero, err
	}
	if op != nil {
		payload, err := json.Marshal(snap)
		if err != nil {
			return zero, err
		}
		payload, err = idempotency.FreezeRenderSnapshot(ctx, s.db, *op, payload)
		if err != nil {
			if idempotency.CodeOf(err) == idempotency.CodeInvalidRequest {
				return zero, &publication.Error{Code: publication.CodeRenderValidation, Message: err.Error(), Err: err}
			}
			return zero, err
		}
		if err = json.Unmarshal(payload, &snap); err != nil {
			return zero, err
		}
	}
	return publication.RenderDetailed(ctx, snap)
}
