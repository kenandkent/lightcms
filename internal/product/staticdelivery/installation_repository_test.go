package staticdelivery

import (
	"path/filepath"
	"testing"
)

func TestInstallationIdentitySurvivesContainerReplacement(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.InstallationID = "already-managed-installation"
	if _, e := r.collection("targets").InsertOne(ctx, target); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	cfg := Config{ProjectRoot: root, Root: filepath.Join(root, "private")}
	id, e := r.InstallationIdentity(ctx, cfg)
	if e != nil || id != target.InstallationID {
		t.Fatalf("must preserve existing managed targets: %q %v", id, e)
	}
	replacement := t.TempDir()
	cfg = Config{ProjectRoot: replacement, Root: filepath.Join(replacement, "private")}
	id, e = r.InstallationIdentity(ctx, cfg)
	if e != nil || id != target.InstallationID {
		t.Fatalf("new container changed identity: %q %v", id, e)
	}
}
