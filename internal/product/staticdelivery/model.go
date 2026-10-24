package staticdelivery

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
	"time"
)

const SchemaVersion = 1
const collectionPrefix = "static_delivery_"

type Record struct {
	ID            primitive.ObjectID `bson:"_id" json:"id"`
	SchemaVersion int                `bson:"schema_version" json:"schema_version"`
	CreatedAt     time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt     time.Time          `bson:"updated_at" json:"updated_at"`
}

type Account struct {
	Record             `bson:",inline"`
	Name               string          `bson:"name" json:"name"`
	AccountID          string          `bson:"account_id" json:"account_id"`
	Status             string          `bson:"status" json:"status"`
	Remark             string          `bson:"remark" json:"remark"`
	CredentialRevision int64           `bson:"credential_revision" json:"credential_revision"`
	Verification       ConnectionCheck `bson:"verification" json:"verification"`
}
type ConnectionCheck struct {
	Readable      bool      `bson:"readable" json:"readable"`
	ListReadable  bool      `bson:"list_readable" json:"list_readable"`
	Subdomain     string    `bson:"subdomain" json:"subdomain,omitempty"`
	WriteVerified bool      `bson:"write_verified" json:"write_verified"`
	CheckedAt     time.Time `bson:"checked_at" json:"checked_at"`
	Code          string    `bson:"code" json:"code,omitempty"`
}
type Credential struct {
	Record     `bson:",inline"`
	AccountRef primitive.ObjectID `bson:"account_ref" json:"account_ref"`
	Revision   int64              `bson:"revision" json:"revision"`
	Token      string             `bson:"token,omitempty" json:"-"`
	Ciphertext []byte             `bson:"ciphertext" json:"-"`
	Nonce      []byte             `bson:"nonce" json:"-"`
	KeyID      string             `bson:"key_id" json:"-"`
	Status     string             `bson:"status" json:"status"`
}
type Target struct {
	Record                           `bson:",inline"`
	AccountRef                       primitive.ObjectID `bson:"account_ref" json:"account_ref"`
	AccountID                        string             `bson:"cf_account_id" json:"-"`
	Name                             string             `bson:"name" json:"name"`
	WorkerName                       string             `bson:"worker_name" json:"worker_name"`
	Environment                      string             `bson:"environment" json:"environment"`
	Origin                           string             `bson:"public_origin" json:"public_origin"`
	ConfigRevision                   int64              `bson:"config_revision" json:"config_revision"`
	ManagementRevision               int64              `bson:"management_revision" json:"management_revision"`
	CurrentRevision                  int64              `bson:"current_revision" json:"current_revision"`
	CurrentReleaseID                 primitive.ObjectID `bson:"current_release_id" json:"current_release_id"`
	ActiveJobID                      primitive.ObjectID `bson:"active_job_id" json:"active_job_id"`
	RemoteVersionID                  string             `bson:"remote_version_id" json:"remote_version_id,omitempty"`
	RemoteDeploymentID               string             `bson:"remote_deployment_id" json:"remote_deployment_id,omitempty"`
	RemoteSettingsHash               string             `bson:"remote_settings_hash" json:"-"`
	RemoteCodeHash                   string             `bson:"remote_code_hash" json:"-"`
	Status                           string             `bson:"status" json:"status"`
	InstallationID                   string             `bson:"installation_id" json:"-"`
	AssetLimit                       int                `bson:"asset_limit" json:"asset_limit"`
	InitializationCredentialRevision int64              `bson:"initialization_credential_revision" json:"-"`
}

type JobState string

const (
	JobQueued            JobState = "queued"
	JobPreparing         JobState = "preparing"
	JobUploading         JobState = "uploading"
	JobVersionReady      JobState = "version_ready"
	JobActivating        JobState = "activating"
	JobVerifying         JobState = "verifying"
	JobSucceeded         JobState = "succeeded"
	JobBlocked           JobState = "blocked"
	JobActivationUnknown JobState = "activation_unknown"
	JobActiveUnverified  JobState = "active_unverified"
	JobFailed            JobState = "failed"
	JobCancelled         JobState = "cancelled"
)

// Transition checks are also required in repository CAS filters. This pure
// function alone is not authority to bypass activation intents or permissions.
func ValidJobTransition(from, to JobState) bool {
	allowed := map[JobState][]JobState{
		JobQueued:            {JobPreparing, JobBlocked, JobCancelled, JobFailed},
		JobPreparing:         {JobUploading, JobBlocked, JobCancelled, JobFailed},
		JobUploading:         {JobVersionReady, JobBlocked, JobCancelled, JobFailed},
		JobVersionReady:      {JobActivating, JobBlocked, JobCancelled, JobFailed},
		JobActivating:        {JobVerifying, JobActivationUnknown},
		JobActivationUnknown: {JobVerifying, JobActiveUnverified, JobBlocked},
		JobVerifying:         {JobSucceeded, JobActiveUnverified},
		JobActiveUnverified:  {JobVerifying, JobBlocked},
		JobBlocked:           {JobQueued, JobCancelled, JobFailed},
		JobFailed:            {JobQueued},
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

type ActorSnapshot struct {
	UserID string   `bson:"user_id" json:"-"`
	Owner  string   `bson:"owner" json:"-"`
	Role   string   `bson:"role" json:"-"`
	Scopes []string `bson:"scopes" json:"-"`
}
type Job struct {
	QueueMode            string             `bson:"queue_mode,omitempty" json:"queue_mode,omitempty"`
	ExpectedPaths        map[string]string  `bson:"expected_paths,omitempty" json:"expected_paths,omitempty"`
	ConfirmedReleaseID   primitive.ObjectID `bson:"confirmed_release_id,omitempty" json:"confirmed_release_id,omitempty"`
	ConfirmedRevision    int64              `bson:"confirmed_revision,omitempty" json:"confirmed_revision,omitempty"`
	Operation            string             `bson:"operation,omitempty" json:"operation,omitempty"`
	CandidateOrigin      string             `bson:"candidate_origin,omitempty" json:"candidate_origin,omitempty"`
	CandidateEnvironment string             `bson:"candidate_environment,omitempty" json:"candidate_environment,omitempty"`
	Record               `bson:",inline"`
	OperationID          primitive.ObjectID   `bson:"operation_id" json:"operation_id"`
	Kind                 string               `bson:"kind" json:"kind"`
	Attempt              int64                `bson:"attempt" json:"attempt"`
	State                JobState             `bson:"state" json:"state"`
	TargetID             primitive.ObjectID   `bson:"target_id,omitempty" json:"target_id,omitempty"`
	ExportID             primitive.ObjectID   `bson:"export_id,omitempty" json:"export_id,omitempty"`
	ExportIDs            []primitive.ObjectID `bson:"export_ids" json:"export_ids"`
	ExpectedRevision     int64                `bson:"expected_revision" json:"expected_revision"`
	ConfigRevision       int64                `bson:"config_revision" json:"config_revision"`
	ReleaseID            primitive.ObjectID   `bson:"release_id" json:"release_id"`
	Actor                ActorSnapshot        `bson:"actor" json:"-"`
	CredentialRef        primitive.ObjectID   `bson:"credential_ref" json:"-"`
	Checkpoint           Checkpoint           `bson:"checkpoint" json:"checkpoint"`
	Heartbeat            time.Time            `bson:"heartbeat" json:"heartbeat"`
	NextRunAt            time.Time            `bson:"next_run_at" json:"next_run_at"`
	CancelRequested      bool                 `bson:"cancel_requested" json:"cancel_requested"`
	ErrorCode            string               `bson:"error_code" json:"error_code,omitempty"`
	LeaseOwner           string               `bson:"lease_owner" json:"-"`
	LeaseUntil           time.Time            `bson:"lease_until" json:"-"`
}
type Checkpoint struct {
	Verification       VerificationProgress `bson:"verification,omitempty" json:"verification,omitempty"`
	ActivationRejected bool                 `bson:"activation_rejected,omitempty" json:"activation_rejected,omitempty"`
	DeadlineExceeded   bool                 `bson:"deadline_exceeded,omitempty" json:"deadline_exceeded,omitempty"`
	ActivationIntentAt *time.Time           `bson:"activation_intent_at,omitempty" json:"activation_intent_at,omitempty"`
	RemoteVersionID    string               `bson:"remote_version_id" json:"remote_version_id,omitempty"`
	PreviousVersionID  string               `bson:"previous_version_id" json:"previous_version_id,omitempty"`
	RemoteDeploymentID string               `bson:"remote_deployment_id" json:"remote_deployment_id,omitempty"`
	OperationMarker    string               `bson:"operation_marker" json:"operation_marker"`
	ConfirmedActive    bool                 `bson:"confirmed_active" json:"confirmed_active"`
}
type Operation struct {
	Record      `bson:",inline"`
	Owner       string             `bson:"owner" json:"-"`
	Action      string             `bson:"action" json:"action"`
	TargetScope string             `bson:"target_scope" json:"target_scope"`
	KeyDigest   string             `bson:"idempotency_key_digest" json:"-"`
	RequestHash string             `bson:"request_hash" json:"-"`
	JobID       primitive.ObjectID `bson:"job_id" json:"job_id"`
	ExportID    primitive.ObjectID `bson:"export_id" json:"export_id"`
	Snapshot    []byte             `bson:"snapshot" json:"-"`
	State       string             `bson:"state" json:"state"`
	ExpiresAt   time.Time          `bson:"expire_at" json:"expire_at"`
}
