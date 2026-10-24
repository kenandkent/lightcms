package staticdelivery

import (
	"context"
	"errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Mongo is authoritative across container replacement. Upgrade the single
// existing target identity rather than minting a new identity that loses its Worker.
func (r *Repository) InstallationIdentity(ctx context.Context, cfg Config) (string, error) {
	var doc struct {
		Value string `bson:"value"`
	}
	e := r.collection("settings").FindOne(ctx, bson.M{"_id": "installation"}).Decode(&doc)
	if e == nil && doc.Value != "" {
		return doc.Value, nil
	}
	if e == nil {
		return "", failure(CodeModuleUnavailable, "installation identity is empty; administrator repair required", nil)
	}
	if e != nil && !errors.Is(e, mongo.ErrNoDocuments) {
		return "", e
	}
	values, e := r.collection("targets").Distinct(ctx, "installation_id", bson.M{"installation_id": bson.M{"$type": "string", "$ne": ""}})
	if e != nil {
		return "", e
	}
	if len(values) > 1 {
		return "", failure(CodeModuleUnavailable, "multiple installation identities require administrator repair", nil)
	}
	value := ""
	if len(values) == 1 {
		value, _ = values[0].(string)
	}
	if value == "" {
		value, e = InstallationID(cfg)
		if e != nil {
			return "", e
		}
	}
	_, e = r.collection("settings").InsertOne(ctx, bson.M{"_id": "installation", "value": value})
	if mongo.IsDuplicateKeyError(e) {
		e = r.collection("settings").FindOne(ctx, bson.M{"_id": "installation"}).Decode(&doc)
		if e == nil && doc.Value == "" {
			return "", failure(CodeModuleUnavailable, "installation identity is empty", nil)
		}
		return doc.Value, e
	}
	return value, e
}
