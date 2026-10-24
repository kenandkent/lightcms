package config

import (
	"os"
	"testing"
)

// Task 16E: publication settings load with spec §36 defaults and validate.
func Test16E_PublicationDefaultsFromEnv(t *testing.T) {
	os.Setenv("MONGO_URI", "mongodb://localhost:27017/test")
	os.Setenv("SESSION_SECRET", "test-secret-12345678")
	os.Unsetenv("PUBLIC_BASE_URL")
	os.Unsetenv("STATIC_STORAGE_PROVIDER")
	os.Unsetenv("PAGE_GENERATION_RATE_LIMIT")
	os.Unsetenv("IDEMPOTENCY_TTL_HOURS")
	os.Unsetenv("IDEMPOTENCY_LEASE_MINUTES")
	os.Unsetenv("PUBLICATION_RETENTION_DAYS")
	os.Unsetenv("PUBLICATION_FAILED_RETENTION_DAYS")
	os.Unsetenv("PUBLICATION_QUARANTINE_RETENTION_DAYS")
	os.Unsetenv("PUBLICATION_SCAN_INTERVAL_MINUTES")
	os.Unsetenv("PUBLICATION_STAGE_TIMEOUT_MINUTES")
	defer func() {
		os.Unsetenv("MONGO_URI")
		os.Unsetenv("SESSION_SECRET")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.PublicBaseURL != cfg.BaseURL {
		t.Errorf("PublicBaseURL should default to BaseURL, got %q", cfg.PublicBaseURL)
	}
	cases := map[string]int{
		"PageGenerationRateLimit":            cfg.PageGenerationRateLimit,
		"IdempotencyTTLHours":                cfg.IdempotencyTTLHours,
		"IdempotencyLeaseMinutes":            cfg.IdempotencyLeaseMinutes,
		"PublicationRetentionDays":           cfg.PublicationRetentionDays,
		"PublicationFailedRetentionDays":     cfg.PublicationFailedRetentionDays,
		"PublicationQuarantineRetentionDays": cfg.PublicationQuarantineRetentionDays,
		"PublicationScanIntervalMinutes":     cfg.PublicationScanIntervalMinutes,
		"PublicationStageTimeoutMinutes":     cfg.PublicationStageTimeoutMinutes,
	}
	wants := map[string]int{
		"PageGenerationRateLimit":            60,
		"IdempotencyTTLHours":                24,
		"IdempotencyLeaseMinutes":            5,
		"PublicationRetentionDays":           90,
		"PublicationFailedRetentionDays":     7,
		"PublicationQuarantineRetentionDays": 30,
		"PublicationScanIntervalMinutes":     10,
		"PublicationStageTimeoutMinutes":     15,
	}
	for name, got := range cases {
		if got != wants[name] {
			t.Errorf("%s = %d, want default %d", name, got, wants[name])
		}
	}
	if cfg.StaticStorageProvider != "filesystem" {
		t.Errorf("StaticStorageProvider = %q, want filesystem", cfg.StaticStorageProvider)
	}
	if err := cfg.ValidatePublicationConfig(); err != nil {
		t.Errorf("default config should validate: %v", err)
	}
}

func Test16E_RejectUnsupportedStorage(t *testing.T) {
	cfg := DefaultDev()
	cfg.ApplyPublicationDefaults()
	cfg.StaticStorageProvider = "s3"
	if err := cfg.ValidatePublicationConfig(); err == nil {
		t.Error("s3 provider must be rejected before its Storage ADR")
	}
	cfg.StaticStorageProvider = "r2"
	if err := cfg.ValidatePublicationConfig(); err == nil {
		t.Error("r2 provider must be rejected before its Storage ADR")
	}
	cfg.StaticStorageProvider = "filesystem"
	cfg.IdempotencyTTLHours = 99
	if err := cfg.ValidatePublicationConfig(); err == nil {
		t.Error("TTL outside 1-72 must be rejected")
	}
}
