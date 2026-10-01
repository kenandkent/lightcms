package services

// Lane 2C fix 4 (red): concurrent theme edits must produce a single
// version chain — strictly increasing, unique, contiguous version numbers.
// The count-based (read count, insert count+1) allocation lets concurrent
// writers read the same count and insert duplicate version numbers.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/database"
)

func TestUpdateTheme_ConcurrentSingleVersionChain(t *testing.T) {
	svc, cleanup := newTestSettingsService(t)
	defer cleanup()
	ctx := context.Background()

	const writers = 10
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			theme := &database.ThemeSettings{
				PrimaryColor: fmt.Sprintf("#%06x", i+1),
				SiteName:     fmt.Sprintf("Site %d", i),
			}
			errs[i] = svc.UpdateTheme(context.Background(), theme)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: UpdateTheme: %v", i, err)
		}
	}

	versions, err := svc.GetThemeVersions(ctx)
	if err != nil {
		t.Fatalf("GetThemeVersions: %v", err)
	}
	// First writer backfills v1 (original) + v2; every writer adds one row.
	if len(versions) != writers+1 {
		t.Fatalf("versions = %d, want %d", len(versions), writers+1)
	}
	nums := make([]int, 0, len(versions))
	for _, v := range versions {
		nums = append(nums, v.Version)
	}
	sort.Ints(nums)
	for i, n := range nums {
		if want := i + 1; n != want {
			t.Fatalf("version chain broken at index %d: got %d, want %d (full chain %v)", i, n, want, nums)
		}
	}
}
