package storage_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestFinalReviewCutoverReplayPreservesCompensationBytes(t *testing.T) {
	s := storage.NewFilesystemStore(t.TempDir())
	ctx := context.Background()
	cid := primitive.NewObjectID()
	oldID, newID := primitive.NewObjectID(), primitive.NewObjectID()
	old, err := s.Stage(ctx, storage.StageRequest{ContentID: cid, PublicationID: oldID, CanonicalPath: "/recover", HTML: []byte("Original page")})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Activate(ctx, old, "/recover"); err != nil {
		t.Fatal(err)
	}
	newObj, err := s.Stage(ctx, storage.StageRequest{ContentID: cid, PublicationID: newID, CanonicalPath: "/recover", HTML: []byte("New page")})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateWithPrevious(ctx, newObj, "/recover", &oldID); err != nil {
		t.Fatal(err)
	}
	// Retry after cutover-before-commit: DB still points to oldID. Repeating
	// the cutover must not replace oldID's backup with the new page bytes.
	if err = s.ActivateWithPrevious(ctx, newObj, "/recover", &oldID); err != nil {
		t.Fatal(err)
	}
	if err = s.CompensateActivate(ctx, newObj, "/recover", &oldID); err != nil {
		t.Fatal(err)
	}
	path, err := s.CanonicalFilePath("/recover")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "Original page" {
		t.Fatalf("compensation restored wrong bytes: %s", got)
	}
}

func TestFinalReviewGuardStopsFinalRename(t *testing.T) {
	s := storage.NewFilesystemStore(t.TempDir())
	ctx := context.Background()
	cid, pid := primitive.NewObjectID(), primitive.NewObjectID()
	obj, err := s.Stage(ctx, storage.StageRequest{ContentID: cid, PublicationID: pid, CanonicalPath: "/guard", HTML: []byte("Unowned page")})
	if err != nil {
		t.Fatal(err)
	}
	guarded := storage.WithWriteGuard(ctx, func() error { return fmt.Errorf("worker lease lost") })
	if err = s.Activate(guarded, obj, "/guard"); err == nil {
		t.Fatal("unowned final rename succeeded")
	}
	path, err := s.CanonicalFilePath("/guard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unowned worker created canonical: %v", err)
	}
}
