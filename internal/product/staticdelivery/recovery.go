package staticdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"log"
	"time"
)

type RecoveryPreview struct {
	Digest    string             `bson:"_id" json:"confirmation_digest"`
	Owner     string             `bson:"owner" json:"-"`
	Target    Target             `bson:"target" json:"target"`
	Remote    RemoteState        `bson:"remote" json:"remote"`
	ReleaseID primitive.ObjectID `bson:"release_id" json:"release_id"`
	JobID     primitive.ObjectID `bson:"job_id" json:"job_id"`
	ExpiresAt time.Time          `bson:"expires_at" json:"expires_at"`
}

func (s *CommandService) recoveryAllowed(ctx context.Context, a ActorSnapshot) error {
	if err := s.authorize(ctx, a, true, false); err != nil {
		return err
	}
	if a.Role != "admin" {
		return failure(CodePermissionDenied, "recovery requires administrator permission", nil)
	}
	if s.options.InspectTarget == nil || s.options.VerifyRecovery == nil || s.store == nil {
		return failure(CodeModuleUnavailable, "recovery verification unavailable", nil)
	}
	return nil
}
func (s *CommandService) recoveryEvidence(ctx context.Context, t Target) (RemoteState, primitive.ObjectID, error) {
	var empty RemoteState
	if !t.ActiveJobID.IsZero() {
		return empty, primitive.NilObjectID, failure(CodeTargetBusy, "active or uncertain deployment must reconcile first", nil)
	}
	if t.Status != "drifted" && t.Status != "ready" {
		return empty, primitive.NilObjectID, failure(CodeTargetNotReady, "target is not recoverable", nil)
	}
	remote, err := s.options.InspectTarget(ctx, t)
	if err != nil {
		return remote, primitive.NilObjectID, err
	}
	id, err := primitive.ObjectIDFromHex(remote.ReleaseID)
	if err != nil || !remote.Exists || !remote.SingleVersion || remote.InstallationID != t.InstallationID || remote.TargetID != t.ID.Hex() || remote.VersionID == "" || t.RemoteSettingsHash != "" && remote.SettingsHash != t.RemoteSettingsHash || t.RemoteCodeHash != "" && remote.CodeHash != t.RemoteCodeHash {
		return remote, id, failure(CodeRemoteDrift, "remote ownership, settings or release cannot be proven", nil)
	}
	// A marker alone is not proof. The immutable version must have been uploaded
	// by a recorded local job for this exact target and captured release.
	if err := s.repo.collection("jobs").FindOne(ctx, bson.M{"target_id": t.ID, "release_id": id, "checkpoint.remote_version_id": remote.VersionID}).Err(); err != nil {
		return remote, id, failure(CodeRemoteDrift, "remote version has no matching local upload evidence", err)
	}
	identity, err := s.releaseIdentity(ctx, id)
	if err != nil {
		return remote, id, err
	}
	if identity.TargetID != t.ID || identity.Origin != t.Origin || identity.Environment != t.Environment || identity.ConfigRevision != t.ConfigRevision {
		return remote, id, failure(CodeRemoteDrift, "remote release configuration differs; no implicit migration", nil)
	}
	return remote, id, nil
}
func (s *CommandService) PreviewRecovery(ctx context.Context, a ActorSnapshot, id primitive.ObjectID) (RecoveryPreview, error) {
	var proof RecoveryPreview
	if err := s.recoveryAllowed(ctx, a); err != nil {
		return proof, err
	}
	target, err := s.repo.GetTarget(ctx, id)
	if err != nil {
		return proof, err
	}
	remote, release, err := s.recoveryEvidence(ctx, target)
	if err != nil {
		return proof, err
	}
	proof = RecoveryPreview{Owner: a.Owner, Target: target, Remote: remote, ReleaseID: release, JobID: primitive.NewObjectID(), ExpiresAt: time.Now().Add(15 * time.Minute)}
	raw, err := json.Marshal(proof)
	if err != nil {
		return proof, err
	}
	proof.Digest = releaseHash(raw)
	err = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if s.repo.references != nil {
			if err := s.repo.references.ReferenceRelease(sc, "recovery-preview:"+proof.Digest, release, &proof.ExpiresAt); err != nil {
				return err
			}
		}
		_, err := s.repo.collection("recovery_previews").InsertOne(sc, proof)
		return err
	})
	return proof, err
}
func (s *CommandService) ConfirmRecovery(ctx context.Context, a ActorSnapshot, id primitive.ObjectID, digest string, confirmed bool) (Job, error) {
	if err := s.recoveryAllowed(ctx, a); err != nil {
		return Job{}, err
	}
	if !confirmed {
		return Job{}, failure(CodeConfirmationRequired, "explicit recovery confirmation required", nil)
	}
	var proof RecoveryPreview
	if err := s.repo.collection("recovery_previews").FindOne(ctx, bson.M{"_id": digest, "owner": a.Owner, "target._id": id}).Decode(&proof); err != nil {
		return Job{}, failure(CodeConfirmationRequired, "recovery preview unavailable", err)
	}
	var replay Job
	if err := s.repo.collection("jobs").FindOne(ctx, bson.M{"_id": proof.JobID, "state": JobSucceeded}).Decode(&replay); err == nil {
		return replay, nil
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		return Job{}, err
	}
	if time.Now().After(proof.ExpiresAt) {
		return Job{}, failure(CodeConfirmationRequired, "recovery preview expired", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if s.options.Admission != nil {
		amount, err := releaseReservation(ctx, s.store, proof.ReleaseID)
		if err != nil {
			return Job{}, err
		}
		release, err := s.options.Admission.Acquire(ctx, amount)
		if err != nil {
			return Job{}, failure(CodeAssetLimitExceeded, "recovery exceeds or is waiting for configured memory capacity", err)
		}
		defer release()
	}
	target, err := s.repo.GetTarget(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if target.CurrentRevision != proof.Target.CurrentRevision || target.ConfigRevision != proof.Target.ConfigRevision || target.AccountRef != proof.Target.AccountRef || target.CurrentReleaseID != proof.Target.CurrentReleaseID {
		return Job{}, failure(CodeRevisionConflict, "target changed after recovery preview", nil)
	}
	remote, release, err := s.recoveryEvidence(ctx, target)
	if err != nil {
		return Job{}, err
	}
	if remote != proof.Remote || release != proof.ReleaseID {
		return Job{}, failure(CodeRemoteDrift, "remote changed after recovery preview", nil)
	}
	manifest, err := s.store.Load(ctx, release)
	if err != nil {
		return Job{}, err
	}
	if manifest.InstallationID != target.InstallationID {
		return Job{}, failure(CodeRemoteDrift, "release belongs to another installation", nil)
	}
	if err := s.options.VerifyRecovery(ctx, manifest); err != nil {
		return Job{}, err
	}
	again, _, err := s.recoveryEvidence(ctx, target)
	if err != nil {
		return Job{}, err
	}
	if again != remote {
		return Job{}, failure(CodeRemoteDrift, "remote changed during verification", nil)
	}
	if err := s.recoveryAllowed(ctx, a); err != nil {
		return Job{}, err
	}
	result := Job{Record: newRecord(), Kind: "recovery", Operation: "recover", State: JobSucceeded, TargetID: id, ReleaseID: release, ExpectedRevision: target.CurrentRevision, ConfigRevision: target.ConfigRevision, Actor: a, Checkpoint: Checkpoint{RemoteVersionID: remote.VersionID, RemoteDeploymentID: remote.DeploymentID, ConfirmedActive: true}}
	result.ID = proof.JobID
	result.OperationID = proof.JobID
	err = s.repo.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if err := s.repo.collection("jobs").FindOne(sc, bson.M{"_id": proof.JobID, "state": JobSucceeded}).Decode(&replay); err == nil {
			result = replay
			return nil
		} else if !errors.Is(err, mongo.ErrNoDocuments) {
			return err
		}
		changed, err := s.repo.collection("targets").UpdateOne(sc, bson.M{"_id": id, "current_revision": target.CurrentRevision, "config_revision": target.ConfigRevision, "current_release_id": target.CurrentReleaseID, "status": target.Status, "$or": bson.A{bson.M{"active_job_id": primitive.NilObjectID}, bson.M{"active_job_id": bson.M{"$exists": false}}}}, bson.M{"$set": bson.M{"status": "ready", "current_release_id": release, "remote_version_id": remote.VersionID, "remote_deployment_id": remote.DeploymentID, "updated_at": time.Now()}, "$inc": bson.M{"current_revision": 1}})
		if err != nil {
			return err
		}
		if changed.MatchedCount != 1 {
			return failure(CodeRevisionConflict, "recovery lost target fence", nil)
		}
		if s.repo.references != nil {
			owner := "delivery-current:" + id.Hex()
			if err := s.repo.references.ReleaseOwner(sc, owner); err != nil {
				return err
			}
			if err := s.repo.references.ReferenceRelease(sc, owner, release, nil); err != nil {
				return err
			}
			if err := s.repo.references.ReleaseOwner(sc, "recovery-preview:"+digest); err != nil {
				return err
			}
		}
		if _, err = s.repo.collection("jobs").InsertOne(sc, result); err != nil {
			return err
		}
		if fence, ok := ctx.Value(recoveryCommitFence{}).(func(mongo.SessionContext, Job) error); ok {
			return fence(sc, result)
		}
		return nil
	})
	if err == nil {
		if refreshErr := s.repo.RefreshPageStatus(ctx, result.ReleaseID); refreshErr != nil {
			log.Printf("delivery recovery status release=%s unavailable: %v", result.ReleaseID.Hex(), refreshErr)
		}
	}
	return result, err
}

func (s *TargetService) InspectTarget(ctx context.Context, t Target) (RemoteState, error) {
	account, err := s.accounts.Get(ctx, t.AccountRef)
	if err != nil {
		return RemoteState{}, err
	}
	if account.Status != "enabled" {
		return RemoteState{}, failure(CodeAccountDisabled, "account disabled", nil)
	}
	remote, err := s.remoteFor(ctx, t, account.CredentialRevision)
	if err != nil {
		return RemoteState{}, err
	}
	return remote.Inspect(ctx, t)
}
