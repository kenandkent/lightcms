package staticdelivery

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func InstallationID(c Config) (string, error) {
	rel, e := filepath.Rel(c.ProjectRoot, c.Root)
	if e != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", failure(CodeModuleUnavailable, "invalid private delivery directory", nil)
	}
	first := strings.ToLower(strings.Split(filepath.ToSlash(rel), "/")[0])
	if first == "static" || first == "content" || first == ".git" {
		return "", failure(CodeModuleUnavailable, "delivery identity must remain private", nil)
	}
	project, e := os.OpenRoot(c.ProjectRoot)
	if e != nil {
		return "", e
	}
	defer project.Close()
	current := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, e := project.Lstat(current)
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", failure(CodeModuleUnavailable, "delivery directory must not traverse symlinks", nil)
		}
	}
	if e = project.MkdirAll(rel, 0700); e != nil {
		return "", e
	}
	root, e := project.OpenRoot(rel)
	if e != nil {
		return "", e
	}
	defer root.Close()
	id := primitive.NewObjectID().Hex()
	f, e := root.OpenFile("installation.id", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e == nil {
		_, e = io.WriteString(f, id)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
		return id, nil
	}
	if !os.IsExist(e) {
		return "", e
	}
	f, e = root.Open("installation.id")
	if e != nil {
		return "", e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 128))
	if e != nil {
		return "", e
	}
	id = strings.TrimSpace(string(raw))
	if _, e = primitive.ObjectIDFromHex(id); e != nil {
		return "", failure(CodeModuleUnavailable, "installation identity file is invalid; restore from backup", nil)
	}
	return id, nil
}
