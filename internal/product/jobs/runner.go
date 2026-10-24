package jobs

import (
	"context"
	"errors"
	"fmt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"log"
	"sort"
	"sync"
	"time"
)

// Retry is only appropriate for operations with a stable external idempotency
// identity. Unknown non-idempotent effects must instead require reconciliation.
type Retry struct {
	After time.Duration
	Code  string
}

func (e *Retry) Error() string { return e.Code }

type Executor struct {
	Concurrency int // Optional process-local limit, leaving slots for other kinds.
	Authorize   func(context.Context, Job) error
	Run         func(context.Context, Job) error
	Finalize    Finalizer
}

type PartialError struct{}

func (*PartialError) Error() string { return "some workflow items failed" }

type ExecutionError struct {
	Code  string
	Cause error
}

func (e *ExecutionError) Error() string { return e.Code }
func (e *ExecutionError) Unwrap() error { return e.Cause }

type Runner struct {
	Repo      *Repository
	mu        sync.RWMutex
	executors map[string]Executor
	active    map[string]int
	slots     chan struct{}
	wake      chan struct{}
	owner     string
}

func NewRunner(repo *Repository, workers int) *Runner {
	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}
	return &Runner{Repo: repo, executors: map[string]Executor{}, active: map[string]int{}, slots: make(chan struct{}, workers), wake: make(chan struct{}, 1), owner: primitive.NewObjectID().Hex()}
}
func (r *Runner) Register(kind string, e Executor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executors[kind] = e
}
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < cap(r.slots); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				r.mu.RLock()
				names := make([]string, 0, len(r.executors))
				for name := range r.executors {
					names = append(names, name)
				}
				r.mu.RUnlock()
				sort.Strings(names)
				for _, name := range names {
					if ctx.Err() != nil {
						return
					}
					if err := r.RunOnce(ctx, name); err != nil {
						log.Printf("workflow runner kind=%s: %v", name, err)
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-r.wake:
				}
			}
		}()
	}
	wg.Wait()
}
func (r *Runner) RunOnce(ctx context.Context, kind string) error {
	r.mu.Lock()
	executor, ok := r.executors[kind]
	if !ok || executor.Run == nil || executor.Authorize == nil {
		r.mu.Unlock()
		return fmt.Errorf("workflow executor requires authorization and execution hooks")
	}
	if executor.Concurrency > 0 && r.active[kind] >= executor.Concurrency {
		r.mu.Unlock()
		return nil
	}
	r.active[kind]++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active[kind]--; r.mu.Unlock() }()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := r.Repo.ExpireWith(ctx, kind, executor.Finalize); err != nil {
		return err
	}
	const lease = time.Minute
	job, err := r.Repo.Claim(ctx, kind, r.owner, lease)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := executor.Authorize(ctx, *job); err != nil {
		return r.Repo.FinishWith(ctx, *job, "failed", "ACTOR_REVOKED", executor.Finalize)
	}
	workCtx, cancel := context.WithDeadline(ctx, job.Deadline)
	defer cancel()
	heartbeatDone := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := r.Repo.Heartbeat(workCtx, *job, lease); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	err = executor.Run(workCtx, *job)
	close(stop)
	<-heartbeatDone
	if ctx.Err() != nil {
		return ctx.Err()
	} // graceful stop leaves the lease for recovery
	if workCtx.Err() != nil {
		return r.Repo.ExpireWith(ctx, kind, executor.Finalize)
	}
	var retry *Retry
	if errors.As(err, &retry) {
		return r.Repo.Requeue(ctx, *job, retry.After, retry.Code)
	}
	var partial *PartialError
	if errors.As(err, &partial) {
		return r.Repo.FinishWith(ctx, *job, "partial", "ITEMS_FAILED", executor.Finalize)
	}
	if err != nil {
		log.Printf("workflow id=%s kind=%s execution failed: %v", job.ID.Hex(), kind, err)
		code := "EXECUTION_FAILED"
		var execution *ExecutionError
		if errors.As(err, &execution) {
			code = execution.Code
		}
		return r.Repo.FinishWith(ctx, *job, "failed", code, executor.Finalize)
	}
	return r.Repo.FinishWith(ctx, *job, "succeeded", "", executor.Finalize)
}
func (r *Repository) Requeue(ctx context.Context, j Job, after time.Duration, code string) error {
	if after < time.Second {
		after = time.Second
	}
	if after > time.Hour {
		after = time.Hour
	}
	result, err := r.DB.Collection("workflow_jobs").UpdateOne(ctx, leaseFilter(j), bson.M{"$set": bson.M{"state": "queued", "next_run_at": time.Now().Add(after), "error_code": code}, "$unset": bson.M{"owner": "", "lease_until": ""}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrLeaseLost
	}
	return nil
}
func (r *Repository) Expire(ctx context.Context, kind string) error {
	return r.ExpireWith(ctx, kind, nil)
}
func (r *Repository) ExpireWith(ctx context.Context, kind string, finalize Finalizer) error {
	cursor, err := r.DB.Collection("workflow_jobs").Find(ctx, bson.M{"kind": kind, "state": bson.M{"$in": bson.A{"queued", "running"}}, "deadline_at": bson.M{"$lte": time.Now()}}, options.Find().SetLimit(100))
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var j Job
		if err := cursor.Decode(&j); err != nil {
			return err
		}
		if err := r.DB.WithTransaction(ctx, func(sc mongo.SessionContext) error {
			state := "failed"
			if j.State == "running" {
				state = "needs_review"
			}
			result, err := r.DB.Collection("workflow_jobs").UpdateOne(sc, bson.M{"_id": j.ID, "state": j.State, "fence": j.Fence, "deadline_at": bson.M{"$lte": time.Now()}}, bson.M{"$set": bson.M{"state": state, "error_code": "DEADLINE_EXCEEDED", "finished_at": time.Now()}, "$inc": bson.M{"fence": 1}})
			if err != nil {
				return err
			}
			if result.MatchedCount == 0 {
				return nil
			}
			if finalize != nil {
				if err := finalize(sc, j, state, "DEADLINE_EXCEEDED"); err != nil {
					return err
				}
			}
			return r.release(sc, j, state, "DEADLINE_EXCEEDED")
		}); err != nil {
			return err
		}
	}
	return cursor.Err()
}
