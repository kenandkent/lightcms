package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"sort"
	"time"
)

const queueMerge = "merge_nonconflicting"

func pageExpectation(page ReleasePage) string {
	return page.ContentID.Hex() + ":" + page.PublicationID.Hex() + ":" + page.SHA256
}
func rebaseQueuedRelease(ctx context.Context, target Target, confirmed ReleaseManifest, current *ReleaseManifest, expected map[string]string) (ReleaseManifest, error) {
	if confirmed.TargetID != target.ID || confirmed.ConfigRevision != target.ConfigRevision || confirmed.Origin != target.Origin || confirmed.Environment != target.Environment || len(expected) == 0 {
		return ReleaseManifest{}, failure(CodeRevisionConflict, "queued target configuration or selection changed", nil)
	}
	paths := make([]string, 0, len(expected))
	for path := range expected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	selected := make([]CapturedPage, 0, len(paths))
	for _, path := range paths {
		actual := ""
		if current != nil {
			if page, ok := current.Pages[path]; ok {
				actual = pageExpectation(page)
			}
		}
		if actual != expected[path] {
			return ReleaseManifest{}, failure(CodeRevisionConflict, "queued page changed; preview again", nil)
		}
		page, ok := confirmed.Pages[path]
		if !ok {
			return ReleaseManifest{}, failure(CodeArtifactUnavailable, "queued frozen page unavailable", nil)
		}
		selected = append(selected, page.Captured)
	}
	return BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Current: current, Pages: selected})
}

func (r *Runner) claimQueuedRelease(ctx context.Context, j *Job, target Target) (bool, error) {
	if !target.ActiveJobID.IsZero() && target.ActiveJobID != j.ID {
		j.NextRunAt = time.Now().Add(2 * time.Second)
		return false, r.save(ctx, j, JobQueued)
	}
	releaseID := j.ReleaseID
	if target.CurrentRevision != j.ExpectedRevision {
		if r.deps.Store == nil {
			return false, failure(CodeModuleUnavailable, "release store unavailable", nil)
		}
		confirmedID := j.ConfirmedReleaseID
		if confirmedID.IsZero() {
			confirmedID = j.ReleaseID
		}
		confirmed, err := r.deps.Store.Load(ctx, confirmedID)
		if err != nil {
			return false, err
		}
		var current *ReleaseManifest
		if !target.CurrentReleaseID.IsZero() {
			snapshot, err := r.deps.Store.Load(ctx, target.CurrentReleaseID)
			if err != nil {
				return false, err
			}
			current = &snapshot
		}
		next, err := rebaseQueuedRelease(ctx, target, confirmed, current, j.ExpectedPaths)
		if err != nil {
			return false, err
		}
		if _, err := ReleaseModules(next); err != nil {
			return false, err
		}
		if len(next.Files) > target.AssetLimit && target.AssetLimit > 0 {
			return false, failure(CodeAssetLimitExceeded, "rebased release exceeds file limit", nil)
		}
		if err := r.deps.Store.Save(ctx, next); err != nil {
			return false, err
		}
		releaseID = next.ID
	}
	err := r.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if err := r.repo.ClaimTarget(sc, target.ID, j.ID, target.CurrentRevision); err != nil {
			return err
		}
		if r.repo.references != nil {
			if err := r.repo.references.ReferenceRelease(sc, "delivery-job:"+j.ID.Hex(), releaseID, nil); err != nil {
				return err
			}
		}
		if releaseID != j.ReleaseID {
			if _, err := r.repo.collection("releases").InsertOne(sc, bson.M{"_id": releaseID, "job_id": j.ID, "target_id": target.ID, "state": "prepared", "created_at": time.Now()}); err != nil {
				return err
			}
		}
		result, err := r.repo.collection("jobs").UpdateOne(sc, bson.M{"_id": j.ID, "state": JobQueued, "lease_owner": j.LeaseOwner, "release_id": j.ReleaseID}, bson.M{"$set": bson.M{"state": JobPreparing, "release_id": releaseID, "expected_revision": target.CurrentRevision, "updated_at": time.Now()}})
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return failure(CodeTargetBusy, "queue lease changed", nil)
		}
		return nil
	})
	if CodeOf(err) == CodeTargetBusy {
		j.NextRunAt = time.Now().Add(2 * time.Second)
		return false, r.save(ctx, j, JobQueued)
	}
	if err != nil {
		return false, err
	}
	j.ReleaseID = releaseID
	j.ExpectedRevision = target.CurrentRevision
	j.State = JobPreparing
	return true, nil
}
