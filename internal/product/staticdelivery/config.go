package staticdelivery

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MemoryBudgetBytes   int64
	ProjectRoot         string
	Root                string
	MasterKey           []byte `json:"-"` // Optional legacy encrypted-credential reads only.
	CredentialError     error  `json:"-"` // Legacy-key diagnostic; never gates plaintext account management.
	GlobalConcurrency   int
	RetentionDays       int
	FailedRetentionDays int
	JobTimeout          time.Duration
}

// LoadConfig is side-effect free. New credentials require no encryption key.
// An optional old key is read only for legacy encrypted credential revisions.
// Root and execution-limit failures disable the new module only.
func LoadConfig(projectRoot string) (Config, error) {
	var c Config
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return c, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return c, fmt.Errorf("static delivery project root: %w", err)
	}
	c.ProjectRoot = root
	raw := strings.TrimSpace(os.Getenv("STATIC_DELIVERY_ROOT"))
	if raw == "" {
		raw = filepath.Join("var", "static-delivery")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(root, raw)
	}
	c.Root, err = prospectivePath(raw)
	if err != nil {
		return c, err
	}
	rel, err := filepath.Rel(root, c.Root)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return c, fmt.Errorf("static delivery root must be a private directory inside the project")
	}
	first := strings.ToLower(strings.Split(filepath.ToSlash(rel), "/")[0])
	if first == "static" || first == "content" || first == ".git" {
		return c, fmt.Errorf("static delivery root must not overlap public content or git metadata")
	}
	for _, limit := range []struct {
		key           string
		dst           *int
		fallback, max int
	}{
		{"GLOBAL_CONCURRENCY", &c.GlobalConcurrency, 2, 32},
		{"RETENTION_DAYS", &c.RetentionDays, 90, 3650},
		{"FAILED_RETENTION_DAYS", &c.FailedRetentionDays, 7, 3650},
	} {
		*limit.dst, err = configInteger(limit.key, limit.fallback, limit.max)
		if err != nil {
			return c, err
		}
	}
	minutes, err := configInteger("JOB_TIMEOUT_MINUTES", 5, 1440)
	if err != nil {
		return c, err
	}
	if minutes > 5 {
		minutes = 5
	} // Preserve old environment files but enforce the five-minute ceiling.
	c.JobTimeout = time.Duration(minutes) * time.Minute
	memoryMiB, memoryErr := configInteger("MEMORY_BUDGET_MIB", 1024, 8192)
	if memoryErr != nil {
		return c, memoryErr
	}
	if memoryMiB < 1024 {
		return c, fmt.Errorf("static delivery memory budget must be at least 1024 MiB for bounded legacy snapshot compatibility")
	}
	c.MemoryBudgetBytes = int64(memoryMiB) << 20
	if encoded := strings.TrimSpace(os.Getenv("STATIC_DELIVERY_MASTER_KEY")); encoded != "" {
		key, e := base64.StdEncoding.DecodeString(encoded)
		if e != nil || len(key) != 32 {
			c.CredentialError = fmt.Errorf("STATIC_DELIVERY_MASTER_KEY must encode exactly 32 bytes")
		} else {
			c.MasterKey = key
		}
	}
	return c, nil
}

func configInteger(key string, fallback, max int) (int, error) {
	value := strings.TrimSpace(os.Getenv("STATIC_DELIVERY_" + key))
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > max {
		return 0, fmt.Errorf("invalid STATIC_DELIVERY_%s (expected 1..%d)", key, max)
	}
	return n, nil
}

// Resolve existing ancestors so a not-yet-created child of a symlink cannot
// bypass the project/public-directory boundary.
func prospectivePath(raw string) (string, error) {
	p, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}
