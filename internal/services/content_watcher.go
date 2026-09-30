package services

import (
	"context"
	"log"
)

// WatchForChanges starts a MongoDB change stream to watch for content updates
// and automatically regenerates static pages when content is modified.
// This enables real-time sync when the database is modified externally (e.g., via MCP).
// The function blocks until the context is cancelled or an error occurs.
// Task 16B (spec §16.6): the watcher must NEVER write canonical live files.
// It is retained as a disabled no-op so legacy boot code keeps compiling;
// index/keyword maintenance happens through the ContentService coalescing
// workers, and live changes happen only via PublicationService.
func (s *ContentService) WatchForChanges(ctx context.Context) {
	log.Println("Content change watcher disabled: V3 requires explicit Publish via PublicationService (no canonical writes from change streams)")
	<-ctx.Done()
	log.Println("Content change watcher stopped")
	return
}

// legacyWatchForChanges preserves the pre-V3 change-stream entry point for
// reference. It performs no live writes: change events are observed for
// logging/alerting only, never for canonical file mutation.
func (s *ContentService) legacyWatchForChanges(ctx context.Context) {
	log.Println("legacy content watcher: observation only, no canonical writes")
	<-ctx.Done()
}
