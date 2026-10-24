package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"time"
)

func (s *ForkService) EnableDurable(runner *jobs.Runner, authorize func(context.Context, jobs.Job) error) {
	s.durable = runner
	s.authorizeMerge = authorize
	runner.Register("fork_merge", jobs.Executor{Authorize: authorize, Run: s.runDurableMerge, Finalize: s.finalizeDurableMerge})
}
func (s *ForkService) SubmitMerge(ctx context.Context, forkID, actorID primitive.ObjectID, email string, publishNew ...bool) (*jobs.Job, error) {
	wantPublish := len(publishNew) > 0 && publishNew[0]
	if s.durable == nil {
		return nil, fmt.Errorf("durable merge is unavailable")
	}
	if err := s.authorizeMerge(ctx, jobs.Job{Kind: "fork_merge", Actor: actorID.Hex(), InputRef: forkID.Hex()}); err != nil {
		return nil, err
	}
	var out *jobs.Job
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var binding struct {
			Status     string              `bson:"status"`
			WorkflowID *primitive.ObjectID `bson:"workflow_id"`
			PublishNew bool                `bson:"workflow_publish_new"`
		}
		if err := s.db.FindOne(sc, "content_forks", bson.M{"_id": forkID}, &binding); err != nil {
			return err
		}
		if binding.WorkflowID != nil {
			if binding.PublishNew != wantPublish {
				return fmt.Errorf("merge already has a frozen publish_new choice")
			}
			if binding.Status != "active" && binding.Status != "partially_merged" && binding.Status != "merged" {
				return fmt.Errorf("fork is %s, cannot merge", binding.Status)
			}
			existing, err := s.durable.Repo.Get(sc, *binding.WorkflowID)
			if err != nil {
				return err
			}
			if err := s.authorizeMerge(sc, *existing); err != nil {
				return err
			}
			if existing.State == "partial" || existing.State == "failed" {
				if err := s.durable.Repo.RetryLocal(sc, existing.ID); err != nil {
					return err
				}
				existing.State = "queued"
			}
			out = existing
			return nil
		}
		if binding.Status != "active" && binding.Status != "partially_merged" {
			return fmt.Errorf("fork is %s, cannot merge", binding.Status)
		}
		pages, err := s.ListPages(sc, forkID)
		if err != nil {
			return err
		}
		inputs := make([]jobs.Input, 0, len(pages))
		for _, page := range pages {
			raw, err := json.Marshal(page)
			if err != nil {
				return err
			}
			prov, _ := ProvenanceFromContext(ctx)
			inputs = append(inputs, jobs.Input{Key: page.ID.Hex(), Data: bson.M{"page": string(raw), "actor_id": actorID.Hex(), "actor_email": email, "publish_new": wantPublish, "author_is_admin": AuthorIsAdminFromContext(ctx), "actor": prov.Actor, "via": prov.Via, "agent_session": prov.AgentSession}})
		}
		out, err = s.durable.Repo.Submit(sc, jobs.Submission{Kind: "fork_merge", Key: forkID.Hex(), Actor: actorID.Hex(), InputRef: forkID.Hex(), Items: inputs})
		if err != nil {
			return err
		}
		return s.db.UpdateOne(sc, "content_forks", bson.M{"_id": forkID}, bson.M{"$set": bson.M{"workflow_id": out.ID, "workflow_actor_email": email, "workflow_publish_new": wantPublish}})
	})
	if err == nil {
		s.durable.Wake()
	}
	return out, err
}
func (s *ForkService) runDurableMerge(ctx context.Context, j jobs.Job) error {
	forkID, err := primitive.ObjectIDFromHex(j.InputRef)
	if err != nil {
		return err
	}
	after := ""
	failed := false
	for {
		items, err := s.durable.Repo.Items(ctx, j.ID, after, 100)
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
			if err := s.authorizeMerge(ctx, j); err != nil {
				return err
			}
			var page models.Content
			raw, _ := item.Input["page"].(string)
			if err := json.Unmarshal([]byte(raw), &page); err != nil {
				return err
			}
			email, _ := item.Input["actor_email"].(string)
			actor, _ := item.Input["actor"].(string)
			via, _ := item.Input["via"].(string)
			session, _ := item.Input["agent_session"].(string)
			itemCtx := WithEditorEmail(ctx, email)
			itemCtx = WithProvenance(itemCtx, Provenance{Actor: actor, Via: via, AgentSession: session})
			admin, _ := item.Input["author_is_admin"].(bool)
			itemCtx = WithAuthorIsAdmin(itemCtx, admin)
			var merged *MergeResult
			encode := func() (bson.M, error) {
				raw, e := bson.Marshal(merged)
				if e != nil {
					return nil, e
				}
				var result bson.M
				e = bson.Unmarshal(raw, &result)
				return result, e
			}
			// Draft checkpoint and publication completion are separate stages.
			// A restart after either stage replays the same publication key.
			err = s.durable.Repo.CommitStage(itemCtx, j, item.Key, false, func(sc mongo.SessionContext) (bson.M, error) {
				var e error
				merged, e = s.mergePageAtomic(sc, forkID, page)
				if e != nil {
					return nil, e
				}
				return encode()
			})
			publishNew, _ := item.Input["publish_new"].(bool)
			if err == nil && publishNew {
				for _, id := range merged.CreatedIDs {
					version := merged.CreatedVersions[id.Hex()]
					if version < 1 {
						return &jobs.ExecutionError{Code: "MERGE_PUBLICATION_VERSION_MISSING"}
					}
					var live models.Content
					if e := s.db.FindOne(itemCtx, "content", bson.M{"_id": id}, &live); e != nil {
						err = e
						break
					}
					if publishErr := s.publishMergedVersion(itemCtx, &live, version); publishErr != nil {
						code := publication.CodeOf(publishErr)
						terminal := errors.Is(publishErr, ErrContentHeld) || errors.Is(publishErr, ErrVersionConflict) || errors.Is(publishErr, errNoMergePublisher)
						switch code {
						case publication.CodeContentVersionConflict, publication.CodeValidationFailed, publication.CodeTemplateNotActive, publication.CodeConflict:
							terminal = true
						}
						if !terminal {
							return &jobs.Retry{After: 5 * time.Second, Code: "MERGE_PUBLICATION_RETRY"}
						}
						merged.NotPublished = append(merged.NotPublished, MergeNotPublished{ID: id, Path: live.FullPath, Reason: publishErr.Error()})
					}
				}
			}
			if err == nil {
				err = s.durable.Repo.CommitItem(itemCtx, j, item.Key, func(mongo.SessionContext) (bson.M, error) { return encode() })
			}

			if err != nil {
				failed = true
				reason := err.Error()
				if saveErr := s.durable.Repo.CommitStage(ctx, j, item.Key, false, func(mongo.SessionContext) (bson.M, error) { return bson.M{"error": reason, "path": page.FullPath}, nil }); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	if failed {
		return &jobs.PartialError{}
	}
	left, err := s.cleanupMergedCopies(ctx, forkID, false)
	if err != nil {
		return err
	}
	if left {
		return &jobs.PartialError{}
	}
	return nil
}
func (s *ForkService) finalizeDurableMerge(ctx mongo.SessionContext, j jobs.Job, state, code string) error {
	id, err := primitive.ObjectIDFromHex(j.InputRef)
	if err != nil {
		return err
	}
	status := "partially_merged"
	if state == "succeeded" {
		status = "merged"
	}
	set := bson.M{"status": status, "merge_error": code}
	var attribution struct {
		Email string `bson:"workflow_actor_email"`
	}
	if err := s.db.FindOne(ctx, "content_forks", bson.M{"_id": id, "workflow_id": j.ID}, &attribution); err != nil {
		return err
	}
	set["merged_by_email"] = attribution.Email
	if status == "merged" {
		set["merged_at"] = time.Now()
		actor, err := primitive.ObjectIDFromHex(j.Actor)
		if err != nil {
			return err
		}
		set["merged_by"] = actor
		result, err := s.DurableMergeResult(ctx, j.ID)
		if err != nil {
			return err
		}
		set["merged_created"] = result.Created
		set["merged_updated"] = result.Updated
		left, err := s.cleanupMergedCopies(ctx, id, true)
		if err != nil {
			return err
		}
		if left {
			set["status"] = "partially_merged"
			set["merge_error"] = "FORK_HAS_NEW_EDITS"
		}
	}
	return s.db.UpdateOne(ctx, "content_forks", bson.M{"_id": id, "status": bson.M{"$in": bson.A{"active", "partially_merged"}}}, bson.M{"$set": set})
}
func (s *ForkService) DurableMergeResult(ctx context.Context, id primitive.ObjectID) (*MergeResult, error) {
	result := &MergeResult{}
	after := ""
	for {
		items, err := s.durable.Repo.Items(ctx, id, after, 100)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			after = item.Key
			if !item.Done {
				reason, _ := item.Result["error"].(string)
				path, _ := item.Result["path"].(string)
				if reason != "" {
					result.Failed = append(result.Failed, MergeFailure{FullPath: path, Error: reason})
				}
				continue
			}
			var one MergeResult
			raw, err := bson.Marshal(item.Result)
			if err != nil {
				return nil, err
			}
			if err := bson.Unmarshal(raw, &one); err != nil {
				return nil, err
			}
			result.Created += one.Created
			result.Updated += one.Updated
			result.CreatedIDs = append(result.CreatedIDs, one.CreatedIDs...)
			if result.CreatedVersions == nil {
				result.CreatedVersions = map[string]int64{}
			}
			for id, version := range one.CreatedVersions {
				result.CreatedVersions[id] = version
			}
			result.UpdatedIDs = append(result.UpdatedIDs, one.UpdatedIDs...)
			result.ContentIDs = append(result.ContentIDs, one.ContentIDs...)
			result.RequiresPublish = append(result.RequiresPublish, one.RequiresPublish...)
			result.Conflicts = append(result.Conflicts, one.Conflicts...)
			result.NotPublished = append(result.NotPublished, one.NotPublished...)
		}
	}
	if len(result.Failed) == 0 {
		job, err := s.durable.Repo.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		preservedEdits := job.State == "partial"
		if job.State == "succeeded" {
			forkID, err := primitive.ObjectIDFromHex(job.InputRef)
			if err != nil {
				return nil, err
			}
			var fork struct {
				MergeError string `bson:"merge_error"`
			}
			if err := s.db.FindOne(ctx, "content_forks", bson.M{"_id": forkID, "workflow_id": id}, &fork); err != nil {
				return nil, err
			}
			preservedEdits = fork.MergeError == "FORK_HAS_NEW_EDITS"
		}
		if preservedEdits {
			result.Failed = append(result.Failed, MergeFailure{Error: "new fork edits were preserved; review before merging again"})
		}
	}
	return result, nil
}

// Legacy synchronous callers retain MergeResult. A disconnected request does
// not cancel the persistent job; the task center can reopen it by ID.
func (s *ForkService) mergeDurablyAndWait(ctx context.Context, forkID, actorID primitive.ObjectID, email string, publishNew ...bool) (*MergeResult, error) {
	job, err := s.SubmitMerge(ctx, forkID, actorID, email, publishNew...)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := s.durable.Repo.Get(ctx, job.ID)
		if err != nil {
			return nil, err
		}
		switch current.State {
		case "succeeded", "partial":
			return s.DurableMergeResult(ctx, job.ID)
		case "failed", "cancelled", "needs_review":
			return nil, fmt.Errorf("merge job %s: %s (%s)", job.ID.Hex(), current.State, current.ErrorCode)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("merge job %s remains persisted: %w", job.ID.Hex(), ctx.Err())
		case <-ticker.C:
		}
	}
}
