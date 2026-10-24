package staticdelivery

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ReleaseStore interface {
	Save(context.Context, ReleaseManifest) error
	Load(context.Context, primitive.ObjectID) (ReleaseManifest, error)
}
type RunnerDependencies struct {
	Admission   *artifacts.Admission
	Owner       string
	Lease       time.Duration
	Concurrency int
	Timeout     time.Duration
	Authorize   func(context.Context, Job, bool) error
	Remote      func(context.Context, Job, Target) (Remote, error)
	Prepare     func(context.Context, Job, Target) (ReleaseManifest, error)
	Store       ReleaseStore
	Verify      func(context.Context, ReleaseManifest) error
	VerifyBatch func(context.Context, ReleaseManifest, VerificationProgress) (VerificationProgress, bool, error)
	Export      func(context.Context, Job) error
}
type Runner struct {
	repo         *Repository
	deps         RunnerDependencies
	accountSlots sync.Map
}

func NewRunner(r *Repository, d RunnerDependencies) *Runner {
	if d.Lease <= 0 {
		d.Lease = 2 * time.Minute
	}
	if d.Concurrency < 1 {
		d.Concurrency = 2
	}
	if d.Concurrency > 32 {
		d.Concurrency = 32
	}
	if d.Timeout <= 0 || d.Timeout > 5*time.Minute {
		d.Timeout = 5 * time.Minute
	}
	return &Runner{repo: r, deps: d}
}
func CanReleaseTarget(state JobState) bool {
	return state == JobSucceeded || state == JobFailed || state == JobCancelled
}
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for n := 0; n < r.deps.Concurrency; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
					slog.Error("static delivery runner failed", "error", err)
				}
				timer := time.NewTimer(2 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	wg.Wait()
}
func (r *Runner) RunOnce(parent context.Context) error {
	if r.repo == nil || r.deps.Owner == "" || r.deps.Authorize == nil || r.deps.Remote == nil {
		return failure(CodeModuleUnavailable, "delivery runner dependencies unavailable", nil)
	}
	if e := r.repo.releaseLegacyFailures(parent); e != nil {
		return e
	}
	now := time.Now().UTC()
	leaseOwner := r.deps.Owner + ":" + primitive.NewObjectID().Hex()
	states := bson.A{JobQueued, JobPreparing, JobUploading, JobVersionReady, JobActivating, JobActivationUnknown, JobVerifying, JobActiveUnverified}
	filter := bson.M{"state": bson.M{"$in": states}, "kind": bson.M{"$ne": "export"}, "$and": bson.A{bson.M{"$or": bson.A{bson.M{"lease_until": bson.M{"$lte": now}}, bson.M{"lease_until": bson.M{"$exists": false}}}}, bson.M{"$or": bson.A{bson.M{"next_run_at": bson.M{"$lte": now}}, bson.M{"next_run_at": bson.M{"$exists": false}}}}}}
	if r.deps.Export != nil {
		delete(filter, "kind")
	}
	var job Job
	e := r.repo.collection("jobs").FindOneAndUpdate(parent, filter, bson.M{"$set": bson.M{"lease_owner": leaseOwner, "lease_until": now.Add(r.deps.Lease), "heartbeat": now}}, options.FindOneAndUpdate().SetReturnDocument(options.After).SetSort(bson.D{{Key: "created_at", Value: 1}})).Decode(&job)
	if errors.Is(e, mongo.ErrNoDocuments) {
		return nil
	}
	if e != nil {
		return e
	}
	deadline := job.CreatedAt.Add(r.deps.Timeout)
	recovering := job.Checkpoint.ActivationIntentAt != nil
	// The age limit survives restarts/takeovers. Recovery is read/reconcile-only
	// after expiration and must retain ownership until the remote truth is known.
	if recovering && time.Now().After(deadline) {
		deadline = time.Now().Add(60 * time.Second)
		job.ErrorCode = CodeJobTimeout
		job.Checkpoint.DeadlineExceeded = true
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		_, _ = r.repo.collection("jobs").UpdateOne(cleanup, bson.M{"_id": job.ID, "lease_owner": leaseOwner}, bson.M{"$set": bson.M{"lease_until": time.Now().UTC(), "lease_owner": ""}})
	}()
	if !recovering && !time.Now().Before(deadline) {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		return r.block(cleanup, &job, failure(CodeJobTimeout, "task exceeded its five-minute completion deadline", nil))
	}
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		period := r.deps.Lease / 3
		if period < 100*time.Millisecond {
			period = 100 * time.Millisecond
		}
		tick := time.NewTicker(period)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				now := time.Now().UTC()
				res, err := r.repo.collection("jobs").UpdateOne(ctx, bson.M{"_id": job.ID, "lease_owner": leaseOwner}, bson.M{"$set": bson.M{"heartbeat": now, "lease_until": now.Add(r.deps.Lease)}})
				if err != nil || res.MatchedCount != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	if r.deps.Admission != nil {
		amount := int64(1 << 30)
		if job.Kind != "export" {
			ids := []primitive.ObjectID{job.ReleaseID}
			if job.QueueMode == queueMerge && job.State == JobQueued {
				target, err := r.repo.GetTarget(ctx, job.TargetID)
				if err != nil {
					return err
				}
				ids = append(ids, target.CurrentReleaseID)
			}
			var err error
			amount, err = releaseReservation(ctx, r.deps.Store, ids...)
			if err != nil {
				return r.block(ctx, &job, err)
			}
		}
		release, err := r.deps.Admission.Acquire(ctx, amount)
		if err != nil {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer stop()
			code := CodeJobTimeout
			if errors.Is(err, artifacts.ErrLimit) {
				code = CodeAssetLimitExceeded
			}
			return r.block(cleanup, &job, failure(code, "task preparation capacity unavailable", err))
		}
		defer release()
	}
	if job.Kind != "export" {
		t, e := r.repo.GetTarget(ctx, job.TargetID)
		if e != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
				defer stop()
				return r.block(cleanup, &job, failure(CodeJobTimeout, "task deadline exceeded", e))
			}
			return e
		}
		value, _ := r.accountSlots.LoadOrStore(t.AccountRef, make(chan struct{}, 2))
		slot := value.(chan struct{})
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
				defer stop()
				return r.block(cleanup, &job, failure(CodeJobTimeout, "task deadline exceeded while waiting for account slot", ctx.Err()))
			}
			return ctx.Err()
		}
	}
	e = r.execute(ctx, &job)
	if e != nil && !CanReleaseTarget(job.State) && errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		// No activation intent: fail and release the target. After intent,
		// preserve uncertainty and allow only reconciliation, never a new POST.
		return r.block(cleanup, &job, failure(CodeJobTimeout, "task exceeded its five-minute completion deadline", e))
	}
	return e
}
func (r *Runner) save(ctx context.Context, j *Job, next JobState) error {
	if next != j.State && !ValidJobTransition(j.State, next) && !(j.Kind == "export" && j.State == JobPreparing && next == JobSucceeded) {
		return failure(CodeInternal, "invalid durable job transition", nil)
	}
	res, e := r.repo.collection("jobs").UpdateOne(ctx, bson.M{"_id": j.ID, "lease_owner": j.LeaseOwner, "state": j.State}, bson.M{"$set": bson.M{"state": next, "checkpoint": j.Checkpoint, "error_code": j.ErrorCode, "next_run_at": j.NextRunAt, "updated_at": time.Now().UTC()}})
	if e != nil {
		return e
	}
	if res.MatchedCount != 1 {
		return failure(CodeTargetBusy, "job ownership or state changed", nil)
	}
	j.State = next
	return nil
}
func (r *Runner) block(ctx context.Context, j *Job, err error) error {
	j.ErrorCode = CodeOf(err)
	var remoteErr *cloudflare.APIError
	if errors.As(err, &remoteErr) {
		slog.ErrorContext(ctx, "static delivery Cloudflare operation failed", "job_id", j.ID.Hex(), "stage", j.State, "error_code", j.ErrorCode, "diagnostic", remoteErr.Error())
	}
	if j.Checkpoint.ActivationIntentAt != nil {
		if j.ErrorCode == CodeJobTimeout {
			j.Checkpoint.DeadlineExceeded = true
		}
		j.NextRunAt = time.Now().UTC().Add(30 * time.Second)
		if j.State == JobActivating {
			return r.save(ctx, j, JobActivationUnknown)
		}
		return r.save(ctx, j, j.State)
	}
	if j.Kind == "export" {
		if j.ErrorCode == CodeJobTimeout {
			return r.save(ctx, j, JobFailed)
		}
		return r.save(ctx, j, JobBlocked)
	}
	return r.repo.failBeforeActivation(ctx, j)
}
func (r *Runner) execute(ctx context.Context, j *Job) error {
	if j.Kind == "export" {
		if e := r.deps.Authorize(ctx, *j, false); e != nil {
			return r.block(ctx, j, e)
		}
		if j.State == JobQueued {
			if e := r.save(ctx, j, JobPreparing); e != nil {
				return e
			}
		}
		if j.State != JobPreparing || r.deps.Export == nil {
			return failure(CodeModuleUnavailable, "export execution unavailable", nil)
		}
		if e := r.deps.Export(ctx, *j); e != nil {
			j.ErrorCode = CodeOf(e)
			return r.save(ctx, j, JobFailed)
		}
		return r.save(ctx, j, JobSucceeded)
	}
	for step := 0; step < 8; step++ {
		if e := ctx.Err(); e != nil {
			return e
		}
		t, e := r.repo.GetTarget(ctx, j.TargetID)
		if e != nil {
			return e
		}
		recovery := j.Checkpoint.ActivationIntentAt != nil || j.State == JobActivating || j.State == JobActivationUnknown || j.State == JobActiveUnverified || j.State == JobVerifying
		// Target commit is authority: repairing the job after commit is read-only.
		if t.CurrentReleaseID == j.ReleaseID && t.CurrentRevision == j.ExpectedRevision+1 {
			if e = r.repo.recordCommittedRelease(ctx, j.ID, j.ReleaseID); e != nil {
				return e
			}
			if j.State != JobVerifying {
				if j.State == JobActivating {
					if e = r.save(ctx, j, JobVerifying); e != nil {
						return e
					}
				} else if j.State == JobActivationUnknown || j.State == JobActiveUnverified {
					if e = r.save(ctx, j, JobVerifying); e != nil {
						return e
					}
				} else {
					return failure(CodeRevisionConflict, "unexpected committed release for job state", nil)
				}
			}
			j.ErrorCode = ""
			return r.save(ctx, j, JobSucceeded)
		}
		if e = r.deps.Authorize(ctx, *j, recovery); e != nil {
			if recovery {
				j.NextRunAt = time.Now().UTC().Add(30 * time.Second)
				return r.save(ctx, j, j.State)
			}
			return r.block(ctx, j, e)
		}
		queueRebase := j.QueueMode == queueMerge && j.State == JobQueued
		if !recovery && (t.Status != "ready" || t.ConfigRevision != j.ConfigRevision || !queueRebase && t.CurrentRevision != j.ExpectedRevision) {
			return r.block(ctx, j, failure(CodeRevisionConflict, "target configuration or release changed", nil))
		}
		if j.CandidateOrigin != "" {
			t.Origin = j.CandidateOrigin
		}
		if j.CandidateEnvironment != "" {
			t.Environment = j.CandidateEnvironment
		}
		if j.State == JobQueued {
			if queueRebase {
				claimed, err := r.claimQueuedRelease(ctx, j, t)
				if err != nil {
					return r.block(ctx, j, err)
				}
				if !claimed {
					return nil
				}
				continue
			}
			if e = r.repo.ClaimTarget(ctx, t.ID, j.ID, j.ExpectedRevision); e != nil {
				return r.block(ctx, j, e)
			}
			if e = r.save(ctx, j, JobPreparing); e != nil {
				return e
			}
			continue
		}
		if t.ActiveJobID != j.ID {
			return failure(CodeTargetBusy, "target belongs to another job", nil)
		}
		if !recovery && (j.State == JobUploading || j.State == JobVersionReady) && r.deps.Store == nil {
			return r.block(ctx, j, failure(CodeModuleUnavailable, "durable release storage unavailable", nil))
		}
		remote, e := r.deps.Remote(ctx, *j, t)
		if e != nil {
			return r.block(ctx, j, e)
		}
		if recovery {
			return r.reconcileActivation(ctx, j, t, remote)
		}
		switch j.State {
		case JobPreparing:
			if r.deps.Prepare == nil || r.deps.Store == nil {
				return r.block(ctx, j, failure(CodeModuleUnavailable, "release preparation unavailable", nil))
			}
			manifest, e := r.deps.Store.Load(ctx, j.ReleaseID)
			if e != nil {
				if !errors.Is(e, os.ErrNotExist) {
					return r.block(ctx, j, e)
				}
				manifest, e = r.deps.Prepare(ctx, *j, t)
				if e != nil {
					return r.block(ctx, j, e)
				}
				if manifest.ID != j.ReleaseID || manifest.TargetID != t.ID {
					return r.block(ctx, j, failure(CodeRevisionConflict, "prepared release identity changed", nil))
				}
				if e = r.deps.Store.Save(ctx, manifest); e != nil {
					return e
				}
			}
			if e = r.save(ctx, j, JobUploading); e != nil {
				return e
			}
		case JobUploading:
			state, e := remote.Inspect(ctx, t)
			if e != nil {
				return r.block(ctx, j, e)
			}
			if !matchesRemote(t, state, t.RemoteVersionID) {
				if e = r.repo.markDrift(ctx, t.ID, j.ID); e != nil {
					return e
				}
				return r.block(ctx, j, failure(CodeRemoteDrift, "remote Worker changed before upload", nil))
			}
			manifest, e := r.deps.Store.Load(ctx, j.ReleaseID)
			if e != nil {
				return r.block(ctx, j, e)
			}
			modules, e := ReleaseModules(manifest)
			if e != nil {
				return r.block(ctx, j, e)
			}
			files, e := ReleaseUploadFiles(manifest)
			if e != nil {
				return r.block(ctx, j, e)
			}
			token, e := remote.UploadAssets(ctx, t, files)
			if e != nil {
				return r.block(ctx, j, e)
			}
			version, e := remote.UploadVersion(ctx, t, token, modules)
			if e != nil {
				return r.block(ctx, j, e)
			}
			j.Checkpoint.RemoteVersionID = version.ID
			j.Checkpoint.PreviousVersionID = state.VersionID
			if e = r.save(ctx, j, JobVersionReady); e != nil {
				return e
			}
		case JobVersionReady:
			// Recheck both authorization and current remote state at the mutation edge.
			if e = r.deps.Authorize(ctx, *j, false); e != nil {
				return r.block(ctx, j, e)
			}
			state, e := remote.Inspect(ctx, t)
			if e != nil {
				return r.block(ctx, j, e)
			}
			if !matchesRemote(t, state, j.Checkpoint.PreviousVersionID) {
				if e = r.repo.markDrift(ctx, t.ID, j.ID); e != nil {
					return e
				}
				return r.block(ctx, j, failure(CodeRemoteDrift, "remote Worker changed before activation", nil))
			}
			if e = r.deps.Authorize(ctx, *j, false); e != nil {
				return r.block(ctx, j, e)
			}
			if e = r.repo.beginActivation(ctx, j, t); e != nil {
				return r.block(ctx, j, e)
			}
			deployment, e := remote.Activate(ctx, t, j.Checkpoint.RemoteVersionID)
			if e != nil {
				var apiErr *cloudflare.APIError
				if errors.As(e, &apiErr) && !apiErr.Uncertain && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
					// An explicit refusal proves this POST did not activate. A
					// timeout/5xx never provides this proof and must reconcile.
					cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer stop()
					return r.repo.failRejectedActivation(cleanup, j)
				}
				j.ErrorCode = CodeCFActivationUnknown
				j.NextRunAt = time.Now().UTC().Add(5 * time.Second)
				return r.save(ctx, j, JobActivationUnknown)
			}
			j.Checkpoint.RemoteDeploymentID = deployment.ID
			if e = r.save(ctx, j, JobVerifying); e != nil {
				return e
			}
		default:
			return failure(CodeInternal, "unsupported runnable delivery state", nil)
		}
	}
	return failure(CodeInternal, "delivery state progression exceeded bound", nil)
}
