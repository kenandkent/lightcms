package migration

// Report is the migration outcome (plan Task 14 interface). DryRun fills the
// diagnostic lists without writing; Run fills them plus the completion and
// index-swap evidence. Blocking lists (invalid slugs, canonical collisions,
// version duplicates, invalid paths, missing files, render errors) must all
// be empty before the flag may move to completed.
type Report struct {
	// DryRun is true for --dry-run analysis (zero writes performed).
	DryRun bool `json:"dry_run"`
	// MigrationFrom/To bracket the system_migrations.publication_model_v1
	// flag transition for this invocation ("", not_started, running,
	// completed; "" = no flag document).
	MigrationFrom string `json:"migration_from"`
	MigrationTo   string `json:"migration_to"`
	// Completed is true only after every serving canonical has an active
	// record, the new canonical index is verified, the legacy index is
	// dropped, and the flag is completed.
	Completed bool `json:"completed"`

	// InvalidSlugs lists template slug defects needing admin repair
	// (empty, illegal pattern, exact duplicates, case-fold collisions).
	// Nothing is silently renamed (spec §34.1).
	InvalidSlugs []SlugIssue `json:"invalid_slugs"`
	// CanonicalCollisions lists canonical keys claimed by >1 live content.
	// Collided pages are skipped until an admin resolves them; the new
	// unique index cannot be created while any remain.
	CanonicalCollisions []Collision `json:"canonical_collisions"`
	// VersionDuplicates lists (content_id, version) history groups with
	// count>1, which block the UNIQUE(content_id, version) index.
	VersionDuplicates []VersionDuplicate `json:"version_duplicates"`
	// InvalidPaths lists live contents whose FullPath fails canonicalization
	// (traversal, empty segment, query/fragment, encoded slash, ...).
	InvalidPaths []PageItem `json:"invalid_paths"`
	// MissingFiles lists serving-expected pages with no canonical file:
	// no active record is created for them (spec §35.3 matrix).
	MissingFiles []PageItem `json:"missing_files"`
	// RenderErrors lists pages whose fresh render failed (old canonical kept
	// serving, no active record) plus apply-time persistence failures
	// (Detail prefixed "migrate: ").
	RenderErrors []PageItem `json:"render_errors"`

	// Verified lists pages whose canonical bytes matched the fresh render
	// (dry-run: candidates; apply: active verified publications created).
	Verified []PageItem `json:"verified"`
	// LegacyUnverified lists pages whose canonical differed from the fresh
	// render (dry-run: candidates; apply: old bytes copied to an immutable
	// legacy object with an active legacy_unverified record; the canonical
	// keeps serving and the scanner never quarantines it).
	LegacyUnverified []PageItem `json:"legacy_unverified"`
	// SkippedActive lists pages that already held an active publication
	// (interrupt-resume path: no duplicate actives created).
	SkippedActive []PageItem `json:"skipped_active"`

	// NewIndexesVerified is true after Task 2's EnsureProductIndexes ran and
	// content_canonical_path_scope_unique was confirmed present.
	NewIndexesVerified bool `json:"new_indexes_verified"`
	// LegacyIndexDropped is true after the old (full_path, fork_id) index
	// was dropped — only ever after NewIndexesVerified.
	LegacyIndexDropped bool `json:"legacy_index_dropped"`
}

// SlugIssue is one template slug defect.
type SlugIssue struct {
	TemplateID   string `json:"template_id"`
	TemplateName string `json:"template_name"`
	Slug         string `json:"slug"`
	Reason       string `json:"reason"`
}

// Collision is one canonical key claimed by multiple live contents.
type Collision struct {
	Canonical  string   `json:"canonical"`
	FullPaths  []string `json:"full_paths"`
	ContentIDs []string `json:"content_ids"`
}

// VersionDuplicate is one (content_id, version) history group with count>1.
type VersionDuplicate struct {
	ContentID string `json:"content_id"`
	Version   int64  `json:"version"`
	Count     int64  `json:"count"`
}

// PageItem is one reconciled (or blocked) page.
type PageItem struct {
	ContentID       string `json:"content_id"`
	FullPath        string `json:"full_path"`
	Canonical       string `json:"canonical,omitempty"`
	ContentVersion  int64  `json:"content_version,omitempty"`
	TemplateVersion int64  `json:"template_version,omitempty"`
	PublicationID   string `json:"publication_id,omitempty"`
	ContentHash     string `json:"content_hash,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

// BlockingCount returns the number of blocking items across all blocking
// categories. Completion requires zero.
func (r Report) BlockingCount() int {
	return len(r.InvalidSlugs) +
		len(r.CanonicalCollisions) +
		len(r.VersionDuplicates) +
		len(r.InvalidPaths) +
		len(r.MissingFiles) +
		len(r.RenderErrors)
}

// HasBlocking reports whether any completion blocker remains.
func (r Report) HasBlocking() bool { return r.BlockingCount() > 0 }
