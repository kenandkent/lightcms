package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFileCannotOverrideExplicitTestIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env.test"), []byte("DATABASE_NAME=wrong-test\nMONGODB_URI=mongodb://wrong.example/test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_NAME", "explicit-isolated-test")
	t.Setenv("MONGODB_URI", "mongodb://127.0.0.1/test")
	loadEnvTest()
	if os.Getenv("DATABASE_NAME") != "explicit-isolated-test" || os.Getenv("MONGODB_URI") != "mongodb://127.0.0.1/test" {
		t.Fatal("test environment was overridden")
	}
}
