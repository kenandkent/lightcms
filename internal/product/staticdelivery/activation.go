package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"log"
	"time"
)

func matchesRemote(t Target, s RemoteState, version string) bool {
	if !ownsTarget(t, s) || version == "" || s.VersionID != version {
		return false
	}
	if t.RemoteSettingsHash != "" && s.SettingsHash != t.RemoteSettingsHash {
		return false
	}
	if t.RemoteCodeHash != "" && s.CodeHash != t.RemoteCodeHash {
		return false
	}
	expectedRelease := ""
	if !t.CurrentReleaseID.IsZero() {
		expectedRelease = t.CurrentReleaseID.Hex()
	}
	return s.ReleaseID == expectedRelease
}
func (r *Repository) markDrift(ctx context.Context, targetID, jobID primitive.ObjectID) error {
	_, e := r.collection("targets").UpdateOne(ctx, bson.M{"_id": targetID, "active_job_id": jobID}, bson.M{"$set": bson.M{"status": "drifted", "updated_at": time.Now().UTC()}})
	return e
}

// The transaction is the authorization/cancellation linearization point. It
// writes the same account and target documents as disable/config mutations.
// No in-memory job state is changed inside the retryable transaction callback.
func (r *Repository) beginActivation(ctx context.Context, j *Job, t Target) error {
	now := time.Now().UTC()
	cp := j.Checkpoint
	cp.ActivationIntentAt = &now
	err := r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if t.AccountRef.IsZero() {
			return failure(CodeAccountDisabled, "activation account is missing", nil)
		}
		res, e := r.collection("accounts").UpdateOne(sc, bson.M{"_id": t.AccountRef, "status": "enabled"}, bson.M{"$set": bson.M{"updated_at": now}})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeAccountDisabled, "account disabled before activation intent", nil)
		}
		res, e = r.collection("targets").UpdateOne(sc, bson.M{"_id": t.ID, "status": "ready", "active_job_id": j.ID, "config_revision": j.ConfigRevision, "current_revision": j.ExpectedRevision}, bson.M{"$set": bson.M{"updated_at": now}})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeRevisionConflict, "target changed before activation intent", nil)
		}
		res, e = r.collection("jobs").UpdateOne(sc, bson.M{"_id": j.ID, "lease_owner": j.LeaseOwner, "state": JobVersionReady, "checkpoint.activation_intent_at": nil}, bson.M{"$set": bson.M{"state": JobActivating, "checkpoint": cp, "updated_at": now}})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeTargetBusy, "job cancelled or lease changed before activation intent", nil)
		}
		return nil
	})
	if err == nil {
		j.State = JobActivating
		j.Checkpoint = cp
	}
	return err
}

func (r *Runner) reconcileActivation(ctx context.Context, j *Job, t Target, remote Remote) error {
	state, e := remote.Inspect(ctx, t)
	if e != nil {
		j.NextRunAt = time.Now().UTC().Add(30 * time.Second)
		return r.save(ctx, j, j.State)
	}
	if !ownsTarget(t, state) || state.VersionID != j.Checkpoint.RemoteVersionID || state.ReleaseID != j.ReleaseID.Hex() {
		// The previous version alone is an expected uncertain observation. A
		// different owner/version, traffic split or changed baseline is drift;
		// retain the job owner so no replacement activation can race recovery.
		if !matchesRemote(t, state, j.Checkpoint.PreviousVersionID) {
			if e = r.repo.markDrift(ctx, t.ID, j.ID); e != nil {
				return e
			}
		}
		// Seeing the old version now is NOT proof a timed-out deployment can never
		// arrive. Preserve the target owner and never repeat POST here.
		j.ErrorCode = CodeCFActivationUnknown
		j.NextRunAt = time.Now().UTC().Add(30 * time.Second)
		if j.State == JobActivating {
			return r.save(ctx, j, JobActivationUnknown)
		}
		return r.save(ctx, j, j.State)
	}
	j.Checkpoint.ConfirmedActive = true
	j.Checkpoint.RemoteDeploymentID = state.DeploymentID
	if j.State != JobVerifying {
		if e = r.save(ctx, j, JobVerifying); e != nil {
			return e
		}
	}
	if r.deps.Store == nil || (r.deps.Verify == nil && r.deps.VerifyBatch == nil) {
		j.ErrorCode = CodeVerificationFailed
		return r.save(ctx, j, JobActiveUnverified)
	}
	var manifest ReleaseManifest
	if summaries, ok := r.deps.Store.(interface {
		Summary(context.Context, primitive.ObjectID) (ReleaseManifest, error)
	}); ok && r.deps.VerifyBatch != nil {
		manifest, e = summaries.Summary(ctx, j.ReleaseID)
	} else {
		manifest, e = r.deps.Store.Load(ctx, j.ReleaseID)
	}
	if e == nil && r.deps.VerifyBatch != nil {
		for {
			previousRound := j.Checkpoint.Verification.Round
			var done bool
			var progress VerificationProgress
			progress, done, e = r.deps.VerifyBatch(ctx, manifest, j.Checkpoint.Verification)
			if e != nil {
				break
			}
			j.Checkpoint.Verification = progress
			j.ErrorCode = ""
			j.NextRunAt = time.Now().UTC().Add(2 * time.Second)
			if err := r.save(ctx, j, JobVerifying); err != nil {
				return err
			}
			if done {
				break
			}
			// Checkpoint every group without adding a scheduler delay per four
			// files. Yield between rounds, or before the current lease budget ends.
			deadline, bounded := ctx.Deadline()
			if progress.Round != previousRound || bounded && time.Until(deadline) < 25*time.Second {
				return nil
			}
		}
	} else if e == nil {
		e = r.deps.Verify(ctx, manifest)
	}
	if e != nil {
		j.ErrorCode = CodeVerificationFailed
		j.NextRunAt = time.Now().UTC().Add(30 * time.Second)
		return r.save(ctx, j, JobActiveUnverified)
	}
	// Re-check remote ownership after the final batch, not only before it.
	final, e := remote.Inspect(ctx, t)
	if e != nil || !ownsTarget(t, final) || final.VersionID != state.VersionID || final.ReleaseID != state.ReleaseID {
		if e == nil {
			j.Checkpoint.Verification = VerificationProgress{}
		}
		j.ErrorCode = CodeVerificationFailed
		j.NextRunAt = time.Now().UTC().Add(30 * time.Second)
		return r.save(ctx, j, JobActiveUnverified)
	}
	if e = r.repo.CommitVerifiedRelease(ctx, t.ID, j.ID, j.ReleaseID, j.ExpectedRevision, state); e != nil {
		return e
	}
	if e = r.repo.recordCommittedRelease(ctx, j.ID, j.ReleaseID); e != nil {
		return e
	}
	j.ErrorCode = ""
	return r.save(ctx, j, JobSucceeded)
}

// Supplementary bookkeeping only, after the target commit authority. A crash
// here is repaired by the runner's already-committed target branch.
func (r *Repository) recordCommittedRelease(ctx context.Context, job, release primitive.ObjectID) error {
	_, e := r.collection("releases").UpdateOne(ctx, bson.M{"_id": release, "job_id": job}, bson.M{"$set": bson.M{"state": "committed", "updated_at": time.Now().UTC()}})
	if e == nil {
		if err := r.RefreshPageStatus(ctx, release); err != nil {
			log.Printf("delivery page status release=%s unavailable: %v", release.Hex(), err)
		}
	}
	return e
}
func (r *Repository) CommitVerifiedRelease(ctx context.Context, targetID, jobID, releaseID primitive.ObjectID, revision int64, state RemoteState) error {
	return r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if r.references != nil {
			owner := "delivery-current:" + targetID.Hex()
			if err := r.references.ReleaseOwner(sc, owner); err != nil {
				return err
			}
			if err := r.references.ReferenceRelease(sc, owner, releaseID, nil); err != nil {
				return err
			}
			if err := r.references.ReleaseOwner(sc, "delivery-job:"+jobID.Hex()); err != nil {
				return err
			}
		}
		return r.commitVerifiedRelease(sc, targetID, jobID, releaseID, revision, state)
	})
}

func (r *Repository) commitVerifiedRelease(ctx context.Context, targetID, jobID, releaseID primitive.ObjectID, revision int64, state RemoteState) error {
	filter := bson.M{"_id": targetID, "active_job_id": jobID, "current_revision": revision}
	set := bson.M{"current_release_id": releaseID, "active_job_id": primitive.NilObjectID, "remote_version_id": state.VersionID, "remote_deployment_id": state.DeploymentID, "remote_settings_hash": state.SettingsHash, "remote_code_hash": state.CodeHash, "updated_at": time.Now().UTC()}
	increment := bson.M{"current_revision": int64(1)}
	var job Job
	err := r.collection("jobs").FindOne(ctx, bson.M{"_id": jobID}).Decode(&job)
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	if job.CandidateOrigin != "" || job.CandidateEnvironment != "" {
		filter["config_revision"] = job.ConfigRevision
		increment["config_revision"] = int64(1)
		if job.CandidateOrigin != "" {
			set["public_origin"] = job.CandidateOrigin
		}
		if job.CandidateEnvironment != "" {
			set["environment"] = job.CandidateEnvironment
		}
	}
	res, e := r.collection("targets").UpdateOne(ctx, filter, bson.M{"$set": set, "$inc": increment})
	if e != nil {
		return e
	}
	if res.MatchedCount == 1 {
		return nil
	}
	t, e := r.GetTarget(ctx, targetID)
	if e == nil && t.CurrentReleaseID == releaseID && t.CurrentRevision == revision+1 {
		return nil
	}
	return failure(CodeRevisionConflict, "verified release lost target ownership", e)
}
func (r *Repository) CancelJob(ctx context.Context, id primitive.ObjectID) error {
	return r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var j Job
		if e := r.collection("jobs").FindOne(sc, bson.M{"_id": id}).Decode(&j); e != nil {
			return e
		}
		if j.State == JobCancelled {
			return nil
		}
		if j.Checkpoint.ActivationIntentAt != nil || j.State == JobActivating || j.State == JobVerifying || j.State == JobActivationUnknown || j.State == JobActiveUnverified {
			return failure(CodeActivationUnknown, "activation intent prevents cancellation; reconcile first", nil)
		}
		if !ValidJobTransition(j.State, JobCancelled) {
			return failure(CodeRevisionConflict, "job cannot be cancelled", nil)
		}
		res, e := r.collection("jobs").UpdateOne(sc, bson.M{"_id": id, "state": j.State, "checkpoint.activation_intent_at": nil}, bson.M{"$set": bson.M{"state": JobCancelled, "lease_owner": "", "lease_until": time.Now().UTC(), "updated_at": time.Now().UTC()}})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeActivationUnknown, "activation raced cancellation", nil)
		}
		_, e = r.collection("targets").UpdateOne(sc, bson.M{"_id": j.TargetID, "active_job_id": id}, bson.M{"$set": bson.M{"active_job_id": primitive.NilObjectID}})
		if e == nil && r.references != nil {
			e = r.references.ReleaseOwner(sc, "delivery-job:"+id.Hex())
		}
		return e
	})
}
