package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"time"
)

// Only the synchronous response to the single activation POST may use this
// path. Observing an old remote version is not evidence of rejection.
func (r *Repository) failRejectedActivation(ctx context.Context, j *Job) error {
	if j.State != JobActivating || j.Checkpoint.ActivationIntentAt == nil {
		return failure(CodeActivationUnknown, "no in-flight activation to reject", nil)
	}
	now := time.Now().UTC()
	cp := j.Checkpoint
	cp.ActivationRejected = true
	const code = "CF_ACTIVATION_REJECTED"
	err := r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		res, err := r.collection("jobs").UpdateOne(sc,
			bson.M{"_id": j.ID, "state": JobActivating, "lease_owner": j.LeaseOwner, "lease_until": bson.M{"$gt": now}, "checkpoint.activation_intent_at": j.Checkpoint.ActivationIntentAt},
			bson.M{"$set": bson.M{"state": JobFailed, "checkpoint": cp, "error_code": code, "lease_owner": "", "lease_until": now, "updated_at": now}})
		if err != nil {
			return err
		}
		if res.MatchedCount != 1 {
			return failure(CodeTargetBusy, "activation lease changed before rejection was saved", nil)
		}
		_, err = r.collection("targets").UpdateOne(sc,
			bson.M{"_id": j.TargetID, "active_job_id": j.ID},
			bson.M{"$set": bson.M{"active_job_id": primitive.NilObjectID, "updated_at": now}})
		if err == nil && r.references != nil {
			err = r.references.ReleaseOwner(sc, "delivery-job:"+j.ID.Hex())
		}
		return err
	})
	if err == nil {
		j.State, j.ErrorCode, j.Checkpoint = JobFailed, code, cp
	}
	return err
}

// Failure and owner release commit together. A crashed/expired worker cannot
// fail a task taken over by another lease, or release another job's target.
func (r *Repository) failBeforeActivation(ctx context.Context, j *Job) error {
	if j.Checkpoint.ActivationIntentAt != nil || !ValidJobTransition(j.State, JobFailed) {
		return failure(CodeActivationUnknown, "cannot fail a task after activation intent", nil)
	}
	now := time.Now().UTC()
	err := r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		filter := bson.M{"_id": j.ID, "state": j.State, "lease_owner": j.LeaseOwner, "checkpoint.activation_intent_at": nil}
		if j.State == JobBlocked {
			filter["lease_until"] = bson.M{"$lte": now}
		}
		res, e := r.collection("jobs").UpdateOne(sc, filter, bson.M{"$set": bson.M{"state": JobFailed, "error_code": j.ErrorCode, "lease_owner": "", "lease_until": now, "updated_at": now}})
		if e != nil {
			return e
		}
		if res.MatchedCount != 1 {
			return failure(CodeTargetBusy, "task lease or activation state changed", nil)
		}
		_, e = r.collection("targets").UpdateOne(sc, bson.M{"_id": j.TargetID, "active_job_id": j.ID}, bson.M{"$set": bson.M{"active_job_id": primitive.NilObjectID, "updated_at": now}})
		if e == nil && r.references != nil {
			e = r.references.ReleaseOwner(sc, "delivery-job:"+j.ID.Hex())
		}
		return e
	})
	if err == nil {
		j.State = JobFailed
	}
	return err
}

// Compatibility recovery for pre-fix blocked tasks. Never touch an active lease
// or a task with durable activation intent. History/error codes are preserved.
func (r *Repository) releaseLegacyFailures(ctx context.Context) error {
	cur, e := r.collection("jobs").Find(ctx, bson.M{"kind": "deploy", "state": JobBlocked, "checkpoint.activation_intent_at": nil, "lease_until": bson.M{"$lte": time.Now().UTC()}})
	if e != nil {
		return e
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var j Job
		if e = cur.Decode(&j); e != nil {
			return e
		}
		if e = r.failBeforeActivation(ctx, &j); e != nil && CodeOf(e) != CodeTargetBusy {
			return e
		}
	}
	return cur.Err()
}
