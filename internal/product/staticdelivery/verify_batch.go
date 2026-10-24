package staticdelivery

import (
	"context"
	"sort"
	"sync"
	"time"
)

// VerificationProgress is persisted with the immutable job release. Each
// cursor counts completed HTTP checks, so takeover never restarts a large site.
type VerificationProgress struct {
	Round  int `bson:"round" json:"round"`
	Offset int `bson:"offset" json:"offset"`
}

// CheckBatch bounds work per recovery lease. Metadata comes from the hashed
// private manifest, while response bytes are independently verified by GET.
func (v *ReleaseVerifier) CheckBatch(ctx context.Context, r ReleaseManifest, progress VerificationProgress) (VerificationProgress, bool, error) {
	// One identity request plus one concurrent group is bounded to 20s,
	// leaving time in the recovery budget to persist the completed cursor.
	client := *v.client
	if client.Timeout == 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	checker := &ReleaseVerifier{client: &client}
	type check struct {
		path   string
		file   ReleaseFile
		head   bool
		absent bool
	}
	paths := map[string]string{}
	for canonical, p := range r.Pages {
		paths[p.File] = canonical
	}
	names := make([]string, 0, len(r.Files))
	for name := range r.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	checks := make([]check, 0, len(names)*2)
	for _, name := range names {
		f := r.Files[name]
		p, page := paths[name]
		if !page {
			p = name
		}
		checks = append(checks, check{path: p, file: f, head: false})
		if page {
			checks = append(checks, check{path: p, file: f, head: true})
		}
	}
	for _, path := range removedURLs(r) {
		checks = append(checks, check{path: path, absent: true}, check{path: path, head: true, absent: true})
	}
	if progress.Round < 0 || progress.Round > 3 || progress.Offset < 0 || progress.Offset > len(checks) {
		return progress, false, failure(CodeVerificationFailed, "invalid verification checkpoint", nil)
	}
	target := Target{Record: Record{ID: r.TargetID}, Origin: r.Origin, InstallationID: r.InstallationID}
	state := RemoteState{InstallationID: r.InstallationID, TargetID: r.TargetID.Hex(), ReleaseID: r.ID.Hex()}
	if err := (&OriginVerifier{client: &client}).Check(ctx, target, state); err != nil {
		return progress, false, failure(CodeVerificationFailed, "public release identity mismatch", err)
	}
	if progress.Round == 3 {
		return progress, true, nil
	}
	end := progress.Offset + 4
	if end > len(checks) {
		end = len(checks)
	}
	var wg sync.WaitGroup
	work := make(chan check)
	errors := make(chan error, 4)
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				var err error
				if c.absent {
					err = checker.checkAbsent(ctx, r, c.path, c.head)
				} else {
					err = checker.checkFile(ctx, r, c.path, c.file, c.head)
				}
				if err != nil {
					select {
					case errors <- err:
					default:
					}
				}
			}
		}()
	}
	for _, c := range checks[progress.Offset:end] {
		work <- c
	}
	close(work)
	wg.Wait()
	close(errors)
	if err := ctx.Err(); err != nil {
		return progress, false, err
	}
	for err := range errors {
		return progress, false, err
	}
	progress.Offset = end
	if end == len(checks) {
		progress.Round++
		progress.Offset = 0
	}
	return progress, progress.Round == 3, nil
}
