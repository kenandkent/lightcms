package staticdelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type OperationRequest struct {
	Owner, Action, TargetScope, Key string
	Body                            json.RawMessage
}

func OperationRequestHash(req OperationRequest) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", failure(CodeInvalidRequest, "invalid JSON request", nil)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", failure(CodeInvalidRequest, "request must contain one JSON value", nil)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func (r *Repository) ReserveOperation(ctx context.Context, req OperationRequest) (Operation, bool, error) {
	var zero Operation
	if strings.TrimSpace(req.Key) == "" {
		return zero, false, failure(CodeIdempotencyRequired, "Idempotency-Key is required", nil)
	}
	if len(req.Key) > 256 || strings.TrimSpace(req.Owner) == "" || strings.TrimSpace(req.Action) == "" {
		return zero, false, failure(CodeInvalidRequest, "invalid operation identity", nil)
	}
	hash, err := OperationRequestHash(req)
	if err != nil {
		return zero, false, err
	}
	key := sha256.Sum256([]byte(req.Key))
	now := time.Now().UTC()
	op := Operation{Record: Record{ID: primitive.NewObjectID(), SchemaVersion: SchemaVersion, CreatedAt: now, UpdatedAt: now},
		Owner: req.Owner, Action: req.Action, TargetScope: req.TargetScope, KeyDigest: hex.EncodeToString(key[:]), RequestHash: hash,
		JobID: primitive.NewObjectID(), ExportID: primitive.NewObjectID(), State: "reserved", ExpiresAt: now.Add(90 * 24 * time.Hour),
	}
	_, err = r.collection("operations").InsertOne(ctx, op)
	if err == nil {
		return op, true, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return zero, false, err
	}
	var existing Operation
	err = r.collection("operations").FindOne(ctx, bson.M{"owner": op.Owner, "action": op.Action, "target_scope": op.TargetScope, "idempotency_key_digest": op.KeyDigest}).Decode(&existing)
	if err != nil {
		return zero, false, err
	}
	if existing.RequestHash != hash {
		return zero, false, failure(CodeIdempotencyConflict, "key was used with a different request", nil)
	}
	return existing, false, nil
}

// FreezeOperation persists a resolved execution snapshot at most once. A
// concurrent replay always receives the winner's version, never its own newer
// resolution. Read paths must not overwrite a snapshot after it is frozen.
func (r *Repository) FreezeOperation(ctx context.Context, id primitive.ObjectID, snapshot []byte) (Operation, error) {
	var result Operation
	if len(snapshot) == 0 || len(snapshot) > 1024*1024 {
		return result, failure(CodeInvalidRequest, "invalid operation snapshot size", nil)
	}
	_, err := r.collection("operations").UpdateOne(ctx, bson.M{"_id": id, "snapshot": nil}, bson.M{"$set": bson.M{"snapshot": snapshot, "state": "frozen", "updated_at": time.Now().UTC()}})
	if err != nil {
		return result, err
	}
	err = r.collection("operations").FindOne(ctx, bson.M{"_id": id}).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return result, failure(CodeNotFound, "operation not found", nil)
	}
	return result, err
}
