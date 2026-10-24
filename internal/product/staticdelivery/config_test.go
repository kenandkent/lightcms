package staticdelivery

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func deliveryConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ROOT", "MASTER_KEY", "GLOBAL_CONCURRENCY", "RETENTION_DAYS", "FAILED_RETENTION_DAYS", "JOB_TIMEOUT_MINUTES"} {
		t.Setenv("STATIC_DELIVERY_"+k, "")
	}
}

// C01: an unconfigured delivery module must still allow offline exports.
func TestConfigWithoutCredentialsAllowsExports(t *testing.T) {
	deliveryConfigEnv(t)
	root := t.TempDir()
	cfg, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	// macOS exposes the temporary directory through /var -> /private/var.
	// LoadConfig deliberately canonicalizes project roots on every platform.
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoot != canonicalRoot || cfg.Root != filepath.Join(canonicalRoot, "var", "static-delivery") || cfg.GlobalConcurrency != 2 || cfg.JobTimeout != 5*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.MasterKey) != 0 {
		t.Fatal("invented a credential encryption key")
	}
	if _, err := os.Stat(cfg.Root); !os.IsNotExist(err) {
		t.Fatal("loading config must not create storage directories")
	}
}

func TestConfigRejectsPublicOrOutsideRoots(t *testing.T) {
	deliveryConfigEnv(t)
	root := t.TempDir()
	for _, name := range []string{".", "..", "static", "static/private", "content/generated/private", "content", ".git/data"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("STATIC_DELIVERY_ROOT", name)
			if _, err := LoadConfig(root); err == nil {
				t.Fatalf("accepted unsafe root %q", name)
			}
		})
	}
}

func TestConfigRejectsSymlinkOutsideProject(t *testing.T) {
	deliveryConfigEnv(t)
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATIC_DELIVERY_ROOT", "linked/exports")
	if _, err := LoadConfig(root); err == nil {
		t.Fatal("symlink escapes project boundary")
	}
}

func TestConfigKeyFailureOnlyDisablesCredentials(t *testing.T) {
	deliveryConfigEnv(t)
	t.Setenv("STATIC_DELIVERY_MASTER_KEY", "invalid-key")
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("bad credential key disabled offline exports: %v", err)
	}
	if cfg.CredentialError == nil || len(cfg.MasterKey) != 0 {
		t.Fatal("invalid key must not become usable")
	}
	t.Setenv("STATIC_DELIVERY_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cfg, err = LoadConfig(t.TempDir())
	if err != nil || cfg.CredentialError != nil || len(cfg.MasterKey) != 32 {
		t.Fatal("valid key rejected")
	}
}

func TestConfigRejectsInvalidLimits(t *testing.T) {
	deliveryConfigEnv(t)
	for _, v := range []string{"-1", "0", "abc", "1000000000000"} {
		t.Setenv("STATIC_DELIVERY_GLOBAL_CONCURRENCY", v)
		if _, err := LoadConfig(t.TempDir()); err == nil {
			t.Fatalf("invalid concurrency %q", v)
		}
	}
}
