package templatecontract

import (
	"context"
	"testing"
)

func TestFinalReviewSlugMigrationIsTransactionalContractChange(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	input := baseInput("slug-before")
	input.Status = "active"
	tpl, old, err := s.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Create(ctx, baseInput("slug-taken")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MigrateSlug(ctx, tpl.ID, 1, "slug-taken"); CodeOf(err) != CodeSlugConflict {
		t.Fatalf("duplicate migration: %v", err)
	}
	unchanged, err := s.GetCurrent(ctx, "slug-before")
	if err != nil || unchanged.Version != 1 {
		t.Fatalf("collision partially moved pointer: %+v %v", unchanged, err)
	}
	if n := versionCount(t, s, tpl.ID); n != 1 {
		t.Fatal("collision inserted a version")
	}
	if _, err = s.MigrateSlug(ctx, tpl.ID, 1, "INVALID"); err == nil {
		t.Fatal("invalid slug accepted")
	}
	current, err := s.MigrateSlug(ctx, tpl.ID, 1, "slug-after")
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != 2 || current.Slug != "slug-after" || current.ContractHash == old.ContractHash {
		t.Fatalf("machine contract did not change: %+v", current)
	}
	historical, err := s.GetVersion(ctx, old.ID)
	if err != nil || historical.Slug != "slug-before" || historical.Version != 1 {
		t.Fatalf("history changed: %+v %v", historical, err)
	}
	if _, err = s.MigrateSlug(ctx, tpl.ID, 1, "slug-stale"); CodeOf(err) != CodeVersionConflict {
		t.Fatalf("stale migration must lose CAS: %v", err)
	}
	stillCurrent, err := s.GetCurrent(ctx, "slug-after")
	if err != nil || stillCurrent.Version != 2 {
		t.Fatalf("stale writer changed current contract: %+v %v", stillCurrent, err)
	}
	if n := versionCount(t, s, tpl.ID); n != 2 {
		t.Fatalf("version count=%d, want exactly 2", n)
	}
}
