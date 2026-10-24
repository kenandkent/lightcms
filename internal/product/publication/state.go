// Lifecycle and storage transition rules for publication records (spec
// §15.6). All functions are pure so the Task 10 scanner and Task 8 saga can
// share them without touching MongoDB.

package publication

// CanTransitionLifecycle reports whether a publication lifecycle edge is
// allowed. The only edges are:
//
//	staged -> active | failed
//	active -> superseded | unpublished
//
// Terminal states (failed, superseded, unpublished) never return to active:
// rollback creates a NEW staged record from retained immutable bytes (or the
// weaker re-render path past retention) and runs the full activation flow.
func CanTransitionLifecycle(from, to Status) bool {
	switch from {
	case StatusStaged:
		return to == StatusActive || to == StatusFailed
	case StatusActive:
		return to == StatusSuperseded || to == StatusUnpublished
	default:
		return false
	}
}

// ValidateLifecycleTransition returns a CodeInvalidTransition error for a
// disallowed edge, nil when allowed.
func ValidateLifecycleTransition(from, to Status) error {
	if CanTransitionLifecycle(from, to) {
		return nil
	}
	return pubErr(CodeInvalidTransition,
		"lifecycle transition "+string(from)+" -> "+string(to)+" is not allowed", nil)
}

// CanTransitionStorage reports whether a storage-state edge is allowed
// (spec §15.6):
//
//	pending -> present | missing
//	present -> deleting | missing | corrupt
//	deleting -> deleted
//	missing | corrupt -> present   (scanner repair from immutable source)
func CanTransitionStorage(from, to StorageState) bool {
	switch from {
	case StoragePending:
		return to == StoragePresent || to == StorageMissing
	case StoragePresent:
		return to == StorageDeleting || to == StorageMissing || to == StorageCorrupt
	case StorageDeleting:
		return to == StorageDeleted
	case StorageMissing, StorageCorrupt:
		return to == StoragePresent
	default:
		return false
	}
}

// ValidateStorageTransition returns a CodeInvalidTransition error for a
// disallowed storage edge, nil when allowed.
func ValidateStorageTransition(from, to StorageState) error {
	if CanTransitionStorage(from, to) {
		return nil
	}
	return pubErr(CodeInvalidTransition,
		"storage transition "+string(from)+" -> "+string(to)+" is not allowed", nil)
}

// ActivatableVerification reports whether a verification state may enter
// active. Verified output always qualifies. legacy_unverified qualifies only
// with allowLegacy=true — the migration path (Task 14) passes true so legacy
// pages stay servable; the ordinary business publish path (Task 8) must pass
// false so unverified legacy bytes can never go live by accident.
func ActivatableVerification(v VerificationStatus, allowLegacy bool) bool {
	switch v {
	case VerificationVerified:
		return true
	case VerificationLegacyUnverified:
		return allowLegacy
	default:
		return false
	}
}
