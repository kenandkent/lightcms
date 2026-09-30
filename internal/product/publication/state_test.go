package publication_test

import (
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
)

// TestStateLifecycleTransitions covers every allowed and disallowed
// publication lifecycle edge from spec §15.6. A failed, superseded, or
// unpublished record must never become active; rollback creates a new record
// (Task 8 owns that path — there is no transition back to active here).
func TestStateLifecycleTransitions(t *testing.T) {
	allowed := [][2]publication.Status{
		{publication.StatusStaged, publication.StatusActive},
		{publication.StatusStaged, publication.StatusFailed},
		{publication.StatusActive, publication.StatusSuperseded},
		{publication.StatusActive, publication.StatusUnpublished},
	}
	for _, tc := range allowed {
		if !publication.CanTransitionLifecycle(tc[0], tc[1]) {
			t.Errorf("expected lifecycle transition %q -> %q to be allowed", tc[0], tc[1])
		}
		if err := publication.ValidateLifecycleTransition(tc[0], tc[1]); err != nil {
			t.Errorf("ValidateLifecycleTransition(%q -> %q) = %v, want nil", tc[0], tc[1], err)
		}
	}

	disallowed := [][2]publication.Status{
		// Terminal states may never become active (rollback = new record).
		{publication.StatusFailed, publication.StatusActive},
		{publication.StatusSuperseded, publication.StatusActive},
		{publication.StatusUnpublished, publication.StatusActive},
		// No other edges exist.
		{publication.StatusStaged, publication.StatusSuperseded},
		{publication.StatusStaged, publication.StatusUnpublished},
		{publication.StatusStaged, publication.StatusStaged},
		{publication.StatusActive, publication.StatusStaged},
		{publication.StatusActive, publication.StatusFailed},
		{publication.StatusActive, publication.StatusActive},
		{publication.StatusFailed, publication.StatusSuperseded},
		{publication.StatusFailed, publication.StatusUnpublished},
		{publication.StatusFailed, publication.StatusFailed},
		{publication.StatusSuperseded, publication.StatusFailed},
		{publication.StatusSuperseded, publication.StatusUnpublished},
		{publication.StatusUnpublished, publication.StatusFailed},
		{publication.StatusUnpublished, publication.StatusSuperseded},
	}
	for _, tc := range disallowed {
		if publication.CanTransitionLifecycle(tc[0], tc[1]) {
			t.Errorf("expected lifecycle transition %q -> %q to be rejected", tc[0], tc[1])
		}
		if err := publication.ValidateLifecycleTransition(tc[0], tc[1]); err == nil {
			t.Errorf("ValidateLifecycleTransition(%q -> %q) = nil, want error", tc[0], tc[1])
		}
	}
}

// TestStateStorageTransitions covers the storage lifecycle from spec §15.6.
// Storage state is independent of publication lifecycle; GC only flips
// storage_state and never rewrites lifecycle history.
func TestStateStorageTransitions(t *testing.T) {
	allowed := [][2]publication.StorageState{
		{publication.StoragePending, publication.StoragePresent},
		{publication.StoragePending, publication.StorageMissing},
		{publication.StoragePresent, publication.StorageDeleting},
		{publication.StorageDeleting, publication.StorageDeleted},
		{publication.StoragePresent, publication.StorageMissing},
		{publication.StoragePresent, publication.StorageCorrupt},
		// Scanner repair from a reliable immutable source.
		{publication.StorageMissing, publication.StoragePresent},
		{publication.StorageCorrupt, publication.StoragePresent},
	}
	for _, tc := range allowed {
		if !publication.CanTransitionStorage(tc[0], tc[1]) {
			t.Errorf("expected storage transition %q -> %q to be allowed", tc[0], tc[1])
		}
		if err := publication.ValidateStorageTransition(tc[0], tc[1]); err != nil {
			t.Errorf("ValidateStorageTransition(%q -> %q) = %v, want nil", tc[0], tc[1], err)
		}
	}

	disallowed := [][2]publication.StorageState{
		{publication.StoragePending, publication.StorageDeleting},
		{publication.StoragePending, publication.StorageDeleted},
		{publication.StoragePending, publication.StorageCorrupt},
		{publication.StoragePending, publication.StoragePending},
		{publication.StoragePresent, publication.StoragePresent},
		{publication.StoragePresent, publication.StorageDeleted},
		{publication.StorageDeleting, publication.StoragePresent},
		{publication.StorageDeleting, publication.StorageMissing},
		{publication.StorageDeleted, publication.StoragePresent},
		{publication.StorageDeleted, publication.StorageDeleting},
		{publication.StorageMissing, publication.StorageDeleted},
		{publication.StorageMissing, publication.StorageCorrupt},
		{publication.StorageCorrupt, publication.StorageDeleted},
		{publication.StorageCorrupt, publication.StorageMissing},
	}
	for _, tc := range disallowed {
		if publication.CanTransitionStorage(tc[0], tc[1]) {
			t.Errorf("expected storage transition %q -> %q to be rejected", tc[0], tc[1])
		}
		if err := publication.ValidateStorageTransition(tc[0], tc[1]); err == nil {
			t.Errorf("ValidateStorageTransition(%q -> %q) = nil, want error", tc[0], tc[1])
		}
	}
}

// TestStateActivationVerification: only verified output may enter active on
// the ordinary business path; migration-approved legacy_unverified is
// servable (Task 14) but requires the explicit allowLegacy opt-in so Task 8
// cannot activate it by accident.
func TestStateActivationVerification(t *testing.T) {
	if !publication.ActivatableVerification(publication.VerificationVerified, false) {
		t.Error("verified must be activatable without allowLegacy")
	}
	if !publication.ActivatableVerification(publication.VerificationVerified, true) {
		t.Error("verified must be activatable with allowLegacy")
	}
	if publication.ActivatableVerification(publication.VerificationLegacyUnverified, false) {
		t.Error("legacy_unverified must NOT be activatable without allowLegacy")
	}
	if !publication.ActivatableVerification(publication.VerificationLegacyUnverified, true) {
		t.Error("legacy_unverified must be activatable with allowLegacy (migration path)")
	}
	if publication.ActivatableVerification(publication.VerificationPending, true) {
		t.Error("pending must never be activatable")
	}
	if publication.ActivatableVerification(publication.VerificationFailed, true) {
		t.Error("failed verification must never be activatable")
	}
}

// TestStateServable: active + verified and active + legacy_unverified stay
// servable; everything else is not served. Storage state never affects the
// control-plane servability decision (data-plane repair is Task 10).
func TestStateServable(t *testing.T) {
	servable := []publication.Publication{
		{Status: publication.StatusActive, VerificationStatus: publication.VerificationVerified},
		{Status: publication.StatusActive, VerificationStatus: publication.VerificationLegacyUnverified},
	}
	for i, p := range servable {
		if !p.IsServable() {
			t.Errorf("case %d (%v/%v) should be servable", i, p.Status, p.VerificationStatus)
		}
	}
	notServable := []publication.Publication{
		{Status: publication.StatusStaged, VerificationStatus: publication.VerificationVerified},
		{Status: publication.StatusActive, VerificationStatus: publication.VerificationPending},
		{Status: publication.StatusActive, VerificationStatus: publication.VerificationFailed},
		{Status: publication.StatusSuperseded, VerificationStatus: publication.VerificationVerified},
		{Status: publication.StatusUnpublished, VerificationStatus: publication.VerificationVerified},
		{Status: publication.StatusFailed, VerificationStatus: publication.VerificationFailed},
	}
	for i, p := range notServable {
		if p.IsServable() {
			t.Errorf("case %d (%v/%v) should NOT be servable", i, p.Status, p.VerificationStatus)
		}
	}
}
