package artifacts

import (
	"context"
	"fmt"
	"mime"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ListQuery only addresses catalog metadata. It cannot select a filesystem path
// or cause the private object/manifest contents to be opened.
type ListQuery struct {
	Search, State, Pin string
	Page, Limit        int
}

type ObjectSummary struct {
	Object
	ReferenceCount      int       `json:"reference_count"`
	ReferencesTruncated bool      `json:"references_truncated"`
	Protected           bool      `json:"protected"`
	CleanupEligible     bool      `json:"cleanup_eligible"`
	RetentionUntil      time.Time `json:"retention_until"`
}

type ObjectPage struct {
	Items   []ObjectSummary `json:"items"`
	Page    int             `json:"page"`
	Limit   int             `json:"limit"`
	HasNext bool            `json:"has_next"`
}

type ObjectReference struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type ObjectDetail struct {
	ObjectSummary
	References        []ObjectReference `json:"references"`
	ProtectionReasons []string          `json:"protection_reasons"`
}

var objectMetadataProjection = bson.M{
	"_id": 1, "size": 1, "mime": 1, "state": 1, "generation": 1,
	"pinned": 1, "failed": 1, "created_at": 1,
}

func managementQuery(q ListQuery) (ListQuery, bson.M, error) {
	q.Search = strings.ToLower(strings.TrimSpace(q.Search))
	if q.Search != "" {
		if len(q.Search) > 64 || strings.Trim(q.Search, "0123456789abcdef") != "" {
			return q, nil, fmt.Errorf("invalid object hash prefix")
		}
	}
	filter := bson.M{}
	if q.Search != "" {
		filter["_id"] = bson.M{"$regex": "^" + q.Search}
	}
	switch q.State {
	case "", "all":
	case "available", "deleting", "deleted":
		filter["state"] = q.State
	case "failed":
		filter["failed"] = true
	default:
		return q, nil, fmt.Errorf("invalid artifact state")
	}
	switch q.Pin {
	case "", "all":
	case "pinned":
		filter["pinned"] = true
	case "unpinned":
		filter["pinned"] = bson.M{"$ne": true}
	default:
		return q, nil, fmt.Errorf("invalid pin filter")
	}
	if q.Page == 0 {
		q.Page = 1
	}
	if q.Page < 1 || q.Page > 1000 {
		return q, nil, fmt.Errorf("artifact page out of range")
	}
	if q.Limit < 1 {
		q.Limit = 25
	}
	if q.Limit > 50 {
		q.Limit = 50
	}
	return q, filter, nil
}

func activeObjectReferences(hash string) bson.M {
	return bson.M{"hash": hash, "$or": bson.A{
		bson.M{"expires_at": bson.M{"$exists": false}}, bson.M{"expires_at": nil},
		bson.M{"expires_at": bson.M{"$gt": time.Now()}},
	}}
}

func (c *Catalog) objectSummary(ctx context.Context, object Object) (ObjectSummary, error) {
	// Strip MIME parameters and reject malformed legacy metadata rather than
	// displaying arbitrary catalog strings as a file type.
	mediaType, _, err := mime.ParseMediaType(object.MIME)
	if err != nil || len(mediaType) > 128 || !strings.Contains(mediaType, "/") {
		mediaType = ""
	}
	object.MIME = mediaType
	n, err := c.DB.Collection("artifact_refs").CountDocuments(ctx, activeObjectReferences(object.Hash), options.Count().SetLimit(101))
	if err != nil {
		return ObjectSummary{}, err
	}
	days := c.RetentionDays
	if object.Failed {
		days = c.FailedRetentionDays
	}
	result := ObjectSummary{Object: object, ReferenceCount: int(n), RetentionUntil: object.CreatedAt.Add(time.Duration(days) * 24 * time.Hour)}
	if n > 100 {
		result.ReferenceCount = 100
		result.ReferencesTruncated = true
	}
	result.Protected = object.Pinned || n > 0
	result.CleanupEligible = c.eligible(object) && n == 0
	return result, nil
}

// ListObjects bounds both the returned metadata and reference counts. It never
// reads Store, release manifests, credentials or remote Cloudflare resources.
func (c *Catalog) ListObjects(ctx context.Context, query ListQuery) (ObjectPage, error) {
	q, filter, err := managementQuery(query)
	page := ObjectPage{Items: []ObjectSummary{}, Page: q.Page, Limit: q.Limit}
	if err != nil {
		return page, err
	}
	cursor, err := c.DB.Collection("artifact_objects").Find(ctx, filter, options.Find().
		SetProjection(objectMetadataProjection).
		SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: 1}}).
		SetSkip(int64((q.Page-1)*q.Limit)).SetLimit(int64(q.Limit+1)))
	if err != nil {
		return page, err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		if len(page.Items) == q.Limit {
			page.HasNext = q.Page < 1000
			break
		}
		var object Object
		if err := cursor.Decode(&object); err != nil {
			return page, err
		}
		item, err := c.objectSummary(ctx, object)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	return page, cursor.Err()
}

func safeObjectReference(ref reference) ObjectReference {
	result := ObjectReference{Kind: "other_reference", ExpiresAt: ref.ExpiresAt}
	prefix, id, _ := strings.Cut(ref.Owner, ":")
	validID := len(id) == 24 && strings.Trim(id, "0123456789abcdef") == ""
	if validID {
		switch prefix {
		case "delivery-current":
			result.Kind = "current_release"
		case "delivery-job":
			result.Kind = "pending_job"
		case "release":
			result.Kind = "release_history"
		case "export-history":
			result.Kind = "export_history"
		}
		if result.Kind != "other_reference" {
			result.ID = id
		}
	} else if prefix == "recovery-preview" {
		result.Kind = "recovery_preview"
	}
	return result
}

// GetObject returns at most 50 active references. Raw reference owners are
// deliberately omitted: only known kinds and validated opaque IDs leave here.
func (c *Catalog) GetObject(ctx context.Context, hash string) (ObjectDetail, error) {
	result := ObjectDetail{References: []ObjectReference{}, ProtectionReasons: []string{}}
	if _, err := objectPath(hash); err != nil {
		return result, err
	}
	var object Object
	if err := c.DB.Collection("artifact_objects").FindOne(ctx, bson.M{"_id": hash}, options.FindOne().SetProjection(objectMetadataProjection)).Decode(&object); err != nil {
		return result, err
	}
	var err error
	result.ObjectSummary, err = c.objectSummary(ctx, object)
	if err != nil {
		return result, err
	}
	if object.Pinned {
		result.ProtectionReasons = append(result.ProtectionReasons, "pinned")
	}
	if result.ReferenceCount > 0 {
		result.ProtectionReasons = append(result.ProtectionReasons, "referenced")
	}
	if object.State != "available" {
		result.ProtectionReasons = append(result.ProtectionReasons, object.State)
	} else if !c.eligible(object) && !object.Pinned {
		result.ProtectionReasons = append(result.ProtectionReasons, "retention")
	}
	cursor, err := c.DB.Collection("artifact_refs").Find(ctx, activeObjectReferences(hash), options.Find().
		SetProjection(bson.M{"owner": 1, "expires_at": 1, "_id": 0}).
		SetSort(bson.D{{Key: "owner", Value: 1}}).SetLimit(51))
	if err != nil {
		return result, err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		if len(result.References) == 50 {
			result.ReferencesTruncated = true
			break
		}
		var ref reference
		if err := cursor.Decode(&ref); err != nil {
			return result, err
		}
		result.References = append(result.References, safeObjectReference(ref))
	}
	return result, cursor.Err()
}
