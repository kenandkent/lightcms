package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/url"
	"sort"
)

func operation(req DeployRequest) string {
	if req.Operation == "" {
		return "publish"
	}
	return req.Operation
}
func validateDeployment(req DeployRequest) error {
	if req.ExpectedConfigRevision < 1 || req.ExpectedRevision < 0 {
		return failure(CodeInvalidRequest, "invalid deployment revisions", nil)
	}
	if !req.ManualScriptReview {
		return failure(CodeConfirmationRequired, "confirm static script and dependency review", nil)
	}
	op := operation(req)
	if req.QueueMode != "" && (req.QueueMode != queueMerge || op != "publish") {
		return failure(CodeInvalidRequest, "queue mode is only supported for non-conflicting page publication", nil)
	}
	if op == "publish" {
		if len(req.ExportIDs) < 1 || len(req.ExportIDs) > 100 {
			return failure(CodeInvalidRequest, "select 1 to 100 exports", nil)
		}
	} else if len(req.ExportIDs) != 0 {
		return failure(CodeInvalidRequest, "maintenance operations do not accept exports", nil)
	}
	if op != "change_origin" && req.NewOrigin != "" || op != "change_environment" && req.NewEnvironment != "" {
		return failure(CodeInvalidRequest, "configuration fields do not match the operation", nil)
	}
	if op != "rollback_pages" && op != "rollback_site" && req.SourceReleaseID != "" {
		return failure(CodeInvalidRequest, "historical release does not match the operation", nil)
	}
	if op != "withdraw" && op != "rollback_pages" && len(req.Paths) > 0 {
		return failure(CodeInvalidRequest, "paths do not match the operation", nil)
	}
	switch op {
	case "publish":
		return nil
	case "withdraw", "rollback_pages":
		if len(req.Paths) < 1 || len(req.Paths) > 100 {
			return failure(CodeInvalidRequest, "select 1 to 100 paths", nil)
		}
		seen := map[string]bool{}
		for _, path := range req.Paths {
			if !validPagePath(path) || seen[path] {
				return failure(CodeInvalidRequest, "invalid or duplicate maintenance path", nil)
			}
			seen[path] = true
		}
		if op == "withdraw" {
			return nil
		}
	case "rollback_site":
	case "change_origin":
		_, err := NormalizeOrigin(req.NewOrigin)
		return err
	case "change_environment":
		if req.NewEnvironment != "test" && req.NewEnvironment != "production" {
			return failure(CodeInvalidRequest, "environment must be test or production", nil)
		}
		return nil
	default:
		return failure(CodeInvalidRequest, "unsupported deployment operation", nil)
	}
	if _, err := primitive.ObjectIDFromHex(req.SourceReleaseID); err != nil {
		return failure(CodeInvalidRequest, "select a historical release", err)
	}
	return nil
}

// Build only from immutable captured documents, never today's template/data.
func lifecycleBuildInput(target Target, req DeployRequest, current, historical *ReleaseManifest) (BuildInput, error) {
	input := BuildInput{ID: primitive.NewObjectID(), Target: target, Current: current}
	if current == nil {
		return input, failure(CodeArtifactUnavailable, "target has no current release", nil)
	}
	switch operation(req) {
	case "withdraw":
		for _, path := range req.Paths {
			if _, ok := current.Pages[path]; !ok {
				return input, failure(CodeNotFound, "withdraw path is not published on this target", nil)
			}
		}
		input.RemovePaths = append([]string(nil), req.Paths...)
	case "rollback_pages", "rollback_site":
		if historical == nil || historical.TargetID != target.ID || historical.InstallationID != target.InstallationID {
			return input, failure(CodePermissionDenied, "historical release belongs to another target or installation", nil)
		}
		paths := append([]string(nil), req.Paths...)
		if operation(req) == "rollback_site" {
			paths = nil
			for path := range historical.Pages {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			retained := *current
			retained.Pages = map[string]ReleasePage{}
			input.Current = &retained
		}
		for _, path := range paths {
			page, ok := historical.Pages[path]
			if !ok {
				return input, failure(CodeNotFound, "historical path is unavailable", nil)
			}
			input.Pages = append(input.Pages, page.Captured)
		}
	case "change_origin":
		origin, err := NormalizeOrigin(req.NewOrigin)
		if err != nil {
			return input, err
		}
		if origin == target.Origin {
			return input, failure(CodeInvalidRequest, "origin is unchanged", nil)
		}
		input.Target.Origin = origin
		input.Target.ConfigRevision++
	case "change_environment":
		if req.NewEnvironment != "test" && req.NewEnvironment != "production" {
			return input, failure(CodeInvalidRequest, "invalid environment", nil)
		}
		if req.NewEnvironment == target.Environment {
			return input, failure(CodeInvalidRequest, "environment is unchanged", nil)
		}
		input.Target.Environment = req.NewEnvironment
		input.Target.ConfigRevision++
	default:
		return input, failure(CodeInvalidRequest, "unsupported maintenance operation", nil)
	}
	return input, nil
}
func lifecycleChanges(current *ReleaseManifest, next ReleaseManifest) ([]PageChange, []string) {
	changes := []PageChange{}
	removed := []string{}
	if current != nil {
		for path, old := range current.Pages {
			now, exists := next.Pages[path]
			if !exists {
				removed = append(removed, path)
				changes = append(changes, PageChange{Path: path, Action: "remove", PublicationID: old.PublicationID, URL: next.Origin + (&url.URL{Path: path}).EscapedPath()})
			} else if old.SHA256 != now.SHA256 || old.PublicationID != now.PublicationID || current.Origin != next.Origin || current.Environment != next.Environment {
				changes = append(changes, PageChange{Path: path, Action: "replace", PublicationID: now.PublicationID, URL: next.Origin + (&url.URL{Path: path}).EscapedPath()})
			}
		}
	}
	for path, page := range next.Pages {
		exists := false
		if current != nil {
			_, exists = current.Pages[path]
		}
		if !exists {
			changes = append(changes, PageChange{Path: path, Action: "add", PublicationID: page.PublicationID, URL: next.Origin + (&url.URL{Path: path}).EscapedPath()})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	sort.Strings(removed)
	return changes, removed
}
func (s *TargetService) CheckCandidateOrigin(ctx context.Context, candidate Target) error {
	if s == nil || s.accounts == nil || s.repo == nil {
		return failure(CodeModuleUnavailable, "origin migration verifier unavailable", nil)
	}
	account, err := s.accounts.Get(ctx, candidate.AccountRef)
	if err != nil {
		return err
	}
	if account.Status != "enabled" {
		return failure(CodeAccountDisabled, "account disabled", nil)
	}
	remote, err := s.remoteFor(ctx, candidate, account.CredentialRevision)
	if err != nil {
		return err
	}
	state, err := remote.Inspect(ctx, candidate)
	if err != nil {
		return err
	}
	if !matchesRemote(candidate, state, candidate.RemoteVersionID) {
		return failure(CodeRemoteDrift, "candidate origin does not belong to the expected Worker version", nil)
	}
	if s.checkOrigin == nil {
		return failure(CodeOriginNotReady, "origin verifier unavailable", nil)
	}
	return s.checkOrigin(ctx, candidate, state)
}
