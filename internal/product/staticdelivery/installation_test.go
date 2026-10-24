package staticdelivery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallationIdentityIsStableAndPrivate(t *testing.T) {
	project := t.TempDir()
	cfg := Config{ProjectRoot: project, Root: filepath.Join(project, "var", "static-delivery")}
	a, e := InstallationID(cfg)
	if e != nil || a == "" {
		t.Fatal(e)
	}
	b, e := InstallationID(cfg)
	if e != nil || a != b {
		t.Fatal("identity changed", e)
	}
	info, e := os.Stat(filepath.Join(cfg.Root, "installation.id"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("identity file mode", e)
	}
	cfg.Root = filepath.Join(project, "static")
	if _, e = InstallationID(cfg); e == nil {
		t.Fatal("public installation file accepted")
	}
}
