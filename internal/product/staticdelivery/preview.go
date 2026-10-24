package staticdelivery

import (
	"encoding/json"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"sort"
)

type Preview struct {
	TargetID           primitive.ObjectID `json:"target_id"`
	BaseRevision       int64              `json:"base_revision"`
	ConfigRevision     int64              `json:"config_revision"`
	Origin             string             `json:"public_origin"`
	Kind               string             `json:"kind"`
	ExportHashes       []string           `json:"export_hashes"`
	RemovePaths        []string           `json:"remove_paths"`
	RestoreReleaseID   primitive.ObjectID `json:"restore_release_id"`
	ManualScriptReview bool               `json:"manual_script_review"`
}

func PreviewDigest(p Preview) string {
	p.ExportHashes = append([]string{}, p.ExportHashes...)
	p.RemovePaths = append([]string{}, p.RemovePaths...)
	sort.Strings(p.ExportHashes)
	sort.Strings(p.RemovePaths)
	b, _ := json.Marshal(p)
	return releaseHash(b)
}
