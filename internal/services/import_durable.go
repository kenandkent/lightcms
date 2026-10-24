package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/services/importer"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type importPlan struct {
	Provenance                                Provenance
	Type                                      models.ImportType
	Records                                   []importer.CSVRecord
	Pages                                     []importer.MarkdownPage
	Feed                                      []importer.FeedItem
	Source                                    *models.ImportSource
	ColumnMap                                 map[string]string
	TitleColumn, SlugColumn, Template, Folder string
	TemplateID                                primitive.ObjectID
	TemplateVersion                           int64
	AutoPublish                               bool
}
type durableImportKey struct{}
type importAuthorityKey struct{}

func WithImportAuthority(ctx context.Context, authority models.ImportAuthority) context.Context {
	return context.WithValue(ctx, importAuthorityKey{}, authority)
}

type durableImportState struct {
	Job              jobs.Job
	Item             jobs.Item
	Plan             importPlan
	Written, Outcome bson.M
}

func (s *ImportService) EnableDurable(runner *jobs.Runner, authorize func(context.Context, jobs.Job) error) {
	s.durable = runner
	s.authorizeImport = authorize
	runner.Register("import", jobs.Executor{Authorize: authorize, Run: s.runDurableImport, Finalize: s.finalizeDurableImport})
}

func (s *ImportService) finalizeDurableImport(ctx mongo.SessionContext, j jobs.Job, state, code string) error {
	id, err := primitive.ObjectIDFromHex(j.InputRef)
	if err != nil {
		return err
	}
	status := models.ImportStatusFailed
	if state == "succeeded" || state == "partial" {
		status = models.ImportStatusDone
	}
	_, err = s.db.Collection("import_jobs").UpdateOne(ctx, bson.M{"_id": id, "status": models.ImportStatusRunning}, bson.M{"$set": bson.M{"status": status, "error_msg": code, "finished_at": time.Now()}})
	return err
}

func (s *ImportService) hydrateImportState(ctx context.Context, job *models.ImportJob) error {
	if s.durable == nil || job.WorkflowID == nil {
		return nil
	}
	workflow, err := s.durable.Repo.Get(ctx, *job.WorkflowID)
	if err != nil {
		return err
	}
	switch workflow.State {
	case "cancelled":
		job.Status = models.ImportStatusCancelled
	case "failed", "needs_review":
		job.Status = models.ImportStatusFailed
		job.ErrorMsg = workflow.ErrorCode
	case "succeeded", "partial":
		job.Status = models.ImportStatusDone
	}
	return nil
}

// Legacy goroutine jobs cannot be reconstructed without their original upload.
// Classify them explicitly on startup rather than silently reimporting content.
func (s *ImportService) ClassifyLegacyImports(ctx context.Context) error {
	_, err := s.db.Collection("import_jobs").UpdateMany(ctx, bson.M{"status": models.ImportStatusRunning, "workflow_id": bson.M{"$exists": false}}, bson.M{"$set": bson.M{"status": models.ImportStatusFailed, "error_msg": "LEGACY_INPUT_UNAVAILABLE: verify completed pages and resubmit the original source", "finished_at": time.Now()}})
	return err
}

func (s *ImportService) submitDurableImport(ctx context.Context, job *models.ImportJob, plan importPlan) (*models.ImportJob, error) {
	plan.Provenance, _ = ProvenanceFromContext(ctx)
	if authority, ok := ctx.Value(importAuthorityKey{}).(models.ImportAuthority); ok {
		authority.Scopes = append([]string(nil), authority.Scopes...)
		job.Authority = &authority
	}
	plans := []importPlan{}
	switch plan.Type {
	case models.ImportTypeCSV:
		for _, row := range plan.Records {
			p := plan
			p.Records = []importer.CSVRecord{row}
			plans = append(plans, p)
		}
	case models.ImportTypeMarkdown:
		for _, page := range plan.Pages {
			p := plan
			p.Pages = []importer.MarkdownPage{page}
			name := importer.FrontmatterGet(page.Frontmatter, "template")
			if name == "" {
				name = p.Template
			}
			if name != "" {
				if template := s.findTemplateByName(ctx, name); template != nil {
					p.TemplateID = template.ID
					p.TemplateVersion = template.CurrentVersion
				}
			}
			p.AutoPublish = p.AutoPublish || importer.FrontmatterGet(page.Frontmatter, "published") == "true"
			plans = append(plans, p)
		}
	case models.ImportTypeRSS:
		plans = append(plans, plan)
	default:
		return nil, fmt.Errorf("unsupported import type")
	}
	for n := range plans {
		p := &plans[n]
		if p.Source != nil {
			p.TemplateID = p.Source.TemplateID
		}
		if !p.TemplateID.IsZero() && p.TemplateVersion == 0 {
			var template models.Template
			if err := s.db.FindOne(ctx, "templates", bson.M{"_id": p.TemplateID}, &template); err == nil {
				p.TemplateVersion = template.CurrentVersion
			} else if !errors.Is(err, mongo.ErrNoDocuments) {
				return nil, err
			}
		}
	}
	inputs, err := freezeImportPlans(plans, 0)
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		if p.AutoPublish || p.Source != nil && p.Source.AutoPublish {
			job.RequiresPublish = true
		}
	}
	actor := job.CreatedBy
	if actor == "" {
		actor = "unattributed"
	}
	err = s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		workflow, err := s.durable.Repo.Submit(sc, jobs.Submission{Kind: "import", Key: job.ID.Hex(), Actor: actor, InputRef: job.ID.Hex(), Items: inputs})
		if err != nil {
			return err
		}
		job.WorkflowID = &workflow.ID
		if _, err := s.db.Collection("import_jobs").InsertOne(sc, job); err != nil {
			return err
		}
		s.db.AfterCommit(sc, func(context.Context) { s.durable.Wake() })
		return nil
	})
	return job, err
}
func freezeImportPlans(plans []importPlan, start int) ([]jobs.Input, error) {
	inputs := make([]jobs.Input, 0, len(plans))
	for i, p := range plans {
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if len(raw) > 1<<20 {
			return nil, fmt.Errorf("one import item exceeds 1 MiB frozen-input limit")
		}
		inputs = append(inputs, jobs.Input{Key: fmt.Sprintf("%08d", start+i), Data: bson.M{"plan": string(raw)}})
	}
	return inputs, nil
}
func (s *ImportService) feedItems(ctx context.Context, url string) ([]importer.FeedItem, error) {
	if state, ok := ctx.Value(durableImportKey{}).(*durableImportState); ok {
		return state.Plan.Feed, nil
	}
	return importer.ParseFeed(ctx, url)
}
func (s *ImportService) runDurableImport(ctx context.Context, workflow jobs.Job) error {
	id, err := primitive.ObjectIDFromHex(workflow.InputRef)
	if err != nil {
		return err
	}
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	after := ""
	for {
		items, err := s.durable.Repo.Items(ctx, workflow.ID, after, 100)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			after = item.Key
			if item.Done {
				continue
			}
			if err := s.authorizeImport(ctx, workflow); err != nil {
				return err
			}
			if err := s.durable.Repo.Heartbeat(ctx, workflow, time.Minute); err != nil {
				return err
			}
			raw, _ := item.Input["plan"].(string)
			var plan importPlan
			if err := json.Unmarshal([]byte(raw), &plan); err != nil {
				return err
			}
			if plan.Type == models.ImportTypeRSS && len(plan.Feed) == 0 {
				if plan.Source == nil {
					return fmt.Errorf("RSS source snapshot missing")
				}
				feed, err := importer.ParseFeed(ctx, plan.Source.URL)
				if err != nil {
					return err
				}
				plans := make([]importPlan, 0, len(feed))
				for _, entry := range feed {
					p := plan
					p.Feed = []importer.FeedItem{entry}
					plans = append(plans, p)
				}
				inputs, err := freezeImportPlans(plans, 1)
				if err != nil {
					return err
				}
				err = s.durable.Repo.CommitItem(ctx, workflow, item.Key, func(sc mongo.SessionContext) (bson.M, error) {
					if err := s.durable.Repo.AppendItems(sc, workflow, inputs); err != nil {
						return nil, err
					}
					return bson.M{"prepared": true}, nil
				})
				if err != nil {
					return err
				}
				continue
			}
			state := &durableImportState{Job: workflow, Item: item, Plan: plan, Written: item.Result}
			workCtx := context.WithValue(ctx, durableImportKey{}, state)
			workCtx = WithProvenance(workCtx, plan.Provenance)
			workCtx = WithAuthorIsAdmin(workCtx, job.Authority != nil && job.Authority.Role == "admin")
			if plan.TemplateVersion > 0 {
				var template models.Template
				if err := s.db.FindOne(ctx, "templates", bson.M{"_id": plan.TemplateID, "current_version": plan.TemplateVersion}, &template); err != nil {
					return &jobs.ExecutionError{Code: "IMPORT_TEMPLATE_CHANGED", Cause: err}
				}
			}
			if _, written := state.Written["content_id"]; !written {
				switch plan.Type {
				case models.ImportTypeCSV:
					s.processCSV(workCtx, job, plan.Records, plan.ColumnMap, plan.TitleColumn, plan.SlugColumn, plan.TemplateID, plan.Folder, plan.AutoPublish)
				case models.ImportTypeMarkdown:
					s.processMarkdown(workCtx, job, plan.Pages, plan.Template, plan.Folder, plan.AutoPublish)
				case models.ImportTypeRSS:
					s.processRSS(workCtx, job, plan.Source, plan.Source.ID)
				}
			}
			output := bson.M{"created": 0, "updated": 0, "failed": 0, "skipped": 0}
			if state.Outcome != nil {
				output = state.Outcome
			}
			if cid, ok := state.Written["content_id"].(string); ok {
				created, _ := state.Written["created"].(bool)
				if created {
					output["created"] = 1
					output["updated"] = 0
				} else {
					output["updated"] = 1
					output["created"] = 0
				}
				output["content_id"] = cid
				publish := plan.AutoPublish
				if plan.Source != nil {
					publish = plan.Source.AutoPublish
				}
				if publish {
					contentID, err := primitive.ObjectIDFromHex(cid)
					if err != nil {
						return err
					}
					version := number64(state.Written["content_version"])
					if legacyPublicationSaga == nil {
						return fmt.Errorf("durable auto-publish requires the publication saga")
					}
					if err := s.contentService.publishInternalVersion(workCtx, contentID, version, "import", "/internal/imports/"+job.ID.Hex()+"/publish", ImportOpKey(job.ID, contentID)); err != nil {
						if errors.Is(err, ErrPublishRetryLater) {
							return &jobs.Retry{After: time.Second, Code: "PUBLICATION_PENDING"}
						}
						output["failed"] = 1
						output["error"] = "AUTO_PUBLISH_FAILED"
					}
				}
			} else if number64(output["failed"]) == 0 {
				output["skipped"] = 1
			}
			if err := s.durable.Repo.CommitItem(ctx, workflow, item.Key, func(mongo.SessionContext) (bson.M, error) { return output, nil }); err != nil {
				return err
			}
		}
	}
	created, updated, failed, total, skipped := 0, 0, 0, 0, 0
	after = ""
	for {
		items, err := s.durable.Repo.Items(ctx, workflow.ID, after, 100)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			after = item.Key
			if item.Result["prepared"] == true {
				continue
			}
			total++
			created += int(number64(item.Result["created"]))
			updated += int(number64(item.Result["updated"]))
			failed += int(number64(item.Result["failed"]))
			skipped += int(number64(item.Result["skipped"]))
		}
	}
	// The legacy job remains a read-compatible projection, not execution authority.
	_, err = s.db.Collection("import_jobs").UpdateOne(ctx, bson.M{"_id": job.ID, "status": models.ImportStatusRunning}, bson.M{"$set": bson.M{"status": models.ImportStatusDone, "total_pages": total, "created": created, "updated": updated, "failed": failed, "skipped": skipped, "finished_at": time.Now()}})
	if err != nil {
		return err
	}
	s.publish(job.ID.Hex(), "data: done|done|import finished\n\n")
	if failed > 0 {
		return &jobs.PartialError{}
	}
	return nil
}
func number64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
