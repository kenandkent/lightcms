// Package config loads LightCMS configuration from environment variables or a JSON config file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds application configuration
type Config struct {
	Port            string `json:"port"`
	MongoURI        string `json:"mongo_uri"`
	Env             string `json:"env"` // "development" or "production"
	SessionSecret   string `json:"session_secret"`
	BaseURL         string `json:"base_url"`          // Public URL of the site (e.g., "https://example.com")
	SecureCookies   bool   `json:"secure_cookies"`    // Set to true in production (requires HTTPS)
	VoyageAPIKey    string `json:"voyage_api_key"`    // Voyage AI API key for semantic search embeddings
	AnthropicAPIKey string `json:"anthropic_api_key"` // Anthropic API key for chat widget answer synthesis
	ResendAPIKey    string `json:"resend_api_key"`    // Resend API key for outbound email (CMS Agent digests)
	EmailFrom       string `json:"email_from"`        // From address for outbound email (e.g. "LightCMS Agent <agent@example.com>")

	// Task 16E: V3 template static publishing settings (spec §36).
	// Zero values select the spec defaults via ApplyPublicationDefaults.
	PublicBaseURL                      string `json:"public_base_url"`                       // Canonical public origin; defaults to BaseURL
	StaticStorageProvider              string `json:"static_storage_provider"`               // MVP: only "filesystem"
	PageGenerationRateLimit            int    `json:"page_generation_rate_limit"`            // Publish generations/minute (default 60)
	IdempotencyTTLHours                int    `json:"idempotency_ttl_hours"`                 // Idempotency record retention (default 24, range 1-72)
	IdempotencyLeaseMinutes            int    `json:"idempotency_lease_minutes"`             // Processing lease (default 5)
	PublicationRetentionDays           int    `json:"publication_retention_days"`            // Superseded/unpublished objects (default 90)
	PublicationFailedRetentionDays     int    `json:"publication_failed_retention_days"`     // Failed objects (default 7)
	PublicationQuarantineRetentionDays int    `json:"publication_quarantine_retention_days"` // Preserved files (default 30)
	PublicationScanIntervalMinutes     int    `json:"publication_scan_interval_minutes"`     // Recovery scanner interval (default 10)
	PublicationStageTimeoutMinutes     int    `json:"publication_stage_timeout_minutes"`     // Stale-stage horizon (default 15)
}

// Publication default values (spec §36; scanner/GC defaults in
// internal/product/publication).
const (
	DefaultStaticStorageProvider              = "filesystem"
	DefaultPageGenerationRateLimit            = 60
	DefaultIdempotencyTTLHours                = 24
	DefaultIdempotencyLeaseMinutes            = 5
	DefaultPublicationRetentionDays           = 90
	DefaultPublicationFailedRetentionDays     = 7
	DefaultPublicationQuarantineRetentionDays = 30
	DefaultPublicationScanIntervalMinutes     = 10
	DefaultPublicationStageTimeoutMinutes     = 15
)

// ApplyPublicationDefaults fills zero-valued publication settings with the
// spec §36 defaults. Call after Load, before server construction.
func (c *Config) ApplyPublicationDefaults() {
	if c.PublicBaseURL == "" {
		c.PublicBaseURL = c.BaseURL
	}
	if c.StaticStorageProvider == "" {
		c.StaticStorageProvider = DefaultStaticStorageProvider
	}
	if c.PageGenerationRateLimit <= 0 {
		c.PageGenerationRateLimit = DefaultPageGenerationRateLimit
	}
	if c.IdempotencyTTLHours <= 0 {
		c.IdempotencyTTLHours = DefaultIdempotencyTTLHours
	}
	if c.IdempotencyLeaseMinutes <= 0 {
		c.IdempotencyLeaseMinutes = DefaultIdempotencyLeaseMinutes
	}
	if c.PublicationRetentionDays <= 0 {
		c.PublicationRetentionDays = DefaultPublicationRetentionDays
	}
	if c.PublicationFailedRetentionDays <= 0 {
		c.PublicationFailedRetentionDays = DefaultPublicationFailedRetentionDays
	}
	if c.PublicationQuarantineRetentionDays <= 0 {
		c.PublicationQuarantineRetentionDays = DefaultPublicationQuarantineRetentionDays
	}
	if c.PublicationScanIntervalMinutes <= 0 {
		c.PublicationScanIntervalMinutes = DefaultPublicationScanIntervalMinutes
	}
	if c.PublicationStageTimeoutMinutes <= 0 {
		c.PublicationStageTimeoutMinutes = DefaultPublicationStageTimeoutMinutes
	}
}

// ValidatePublicationConfig rejects unsupported production configurations
// at startup (spec §36): only the filesystem store is approved for MVP
// (R2/S3 need a separate Storage ADR + integration suite), and the
// idempotency TTL must stay inside the 1–72h policy window.
func (c *Config) ValidatePublicationConfig() error {
	if c.StaticStorageProvider != DefaultStaticStorageProvider {
		return fmt.Errorf("unsupported STATIC_STORAGE_PROVIDER %q: MVP production supports only %q (R2/S3 require a Storage ADR and full integration suite)",
			c.StaticStorageProvider, DefaultStaticStorageProvider)
	}
	if c.IdempotencyTTLHours < 1 || c.IdempotencyTTLHours > 72 {
		return fmt.Errorf("IDEMPOTENCY_TTL_HOURS must be 1-72 (got %d)", c.IdempotencyTTLHours)
	}
	if c.IdempotencyLeaseMinutes < 1 {
		return fmt.Errorf("IDEMPOTENCY_LEASE_MINUTES must be >= 1 (got %d)", c.IdempotencyLeaseMinutes)
	}
	return nil
}

// DefaultDev returns default development configuration
func DefaultDev() *Config {
	return &Config{
		Port:          "8082",
		MongoURI:      "", // Must be set in config file
		Env:           "development",
		SessionSecret: "dev-session-secret-change-in-prod",
		BaseURL:       "http://localhost:8082",
		SecureCookies: false,
	}
}

// DefaultProd returns default production configuration
func DefaultProd() *Config {
	return &Config{
		Port:          "80",
		MongoURI:      "", // Must be set in config file
		Env:           "production",
		SessionSecret: "", // Must be set in config file
		BaseURL:       "", // Must be set in config file
		SecureCookies: true,
	}
}

// Load loads configuration from JSON config file or environment variables
// Priority: environment variables > config.prod.json > config.dev.json
// Set LIGHTCMS_CONFIG_DIR to specify a custom config directory
func Load() (*Config, error) {
	// Check if running with environment variables (e.g., Fly.io)
	if mongoURI := os.Getenv("MONGO_URI"); mongoURI != "" {
		return loadFromEnv()
	}

	// Fall back to config files
	var configPath string
	var cfg *Config

	// Check for custom config directory
	configDir := os.Getenv("LIGHTCMS_CONFIG_DIR")
	if configDir == "" {
		configDir = "."
	}

	// Check for production config first
	prodPath := filepath.Join(configDir, "config.prod.json")
	devPath := filepath.Join(configDir, "config.dev.json")

	if _, err := os.Stat(prodPath); err == nil {
		configPath = prodPath
		cfg = DefaultProd()
	} else if _, err := os.Stat(devPath); err == nil {
		configPath = devPath
		cfg = DefaultDev()
	} else {
		return nil, fmt.Errorf("no config file found (expected config.dev.json or config.prod.json in %s)", configDir)
	}

	if err := loadFromFile(configPath, cfg); err != nil {
		return nil, fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}
	cfg.ApplyPublicationDefaults()

	return cfg, nil
}

// loadFromEnv loads configuration from environment variables (for Fly.io deployment)
func loadFromEnv() (*Config, error) {
	cfg := DefaultProd()

	cfg.MongoURI = os.Getenv("MONGO_URI")
	if cfg.MongoURI == "" {
		return nil, fmt.Errorf("MONGO_URI environment variable is required")
	}

	cfg.SessionSecret = os.Getenv("SESSION_SECRET")
	if cfg.SessionSecret == "" {
		return nil, fmt.Errorf("SESSION_SECRET environment variable is required")
	}

	cfg.BaseURL = os.Getenv("BASE_URL")
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://lightcms.fly.dev" // Default Fly.io URL
	}

	if port := os.Getenv("PORT"); port != "" {
		cfg.Port = port
	}

	if env := os.Getenv("ENV"); env != "" {
		cfg.Env = env
	}

	// SecureCookies defaults to true in production
	if secure := os.Getenv("SECURE_COOKIES"); secure == "false" {
		cfg.SecureCookies = false
	}

	cfg.VoyageAPIKey = os.Getenv("VOYAGE_API_KEY")
	cfg.ResendAPIKey = os.Getenv("RESEND_API_KEY")
	cfg.EmailFrom = os.Getenv("EMAIL_FROM")
	cfg.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")

	// Task 16E: V3 publication settings (spec §36). Empty/unset keeps the
	// zero value; ApplyPublicationDefaults fills spec defaults.
	cfg.PublicBaseURL = os.Getenv("PUBLIC_BASE_URL")
	cfg.StaticStorageProvider = os.Getenv("STATIC_STORAGE_PROVIDER")
	cfg.PageGenerationRateLimit = atoiEnv("PAGE_GENERATION_RATE_LIMIT", 0)
	cfg.IdempotencyTTLHours = atoiEnv("IDEMPOTENCY_TTL_HOURS", 0)
	cfg.IdempotencyLeaseMinutes = atoiEnv("IDEMPOTENCY_LEASE_MINUTES", 0)
	cfg.PublicationRetentionDays = atoiEnv("PUBLICATION_RETENTION_DAYS", 0)
	cfg.PublicationFailedRetentionDays = atoiEnv("PUBLICATION_FAILED_RETENTION_DAYS", 0)
	cfg.PublicationQuarantineRetentionDays = atoiEnv("PUBLICATION_QUARANTINE_RETENTION_DAYS", 0)
	cfg.PublicationScanIntervalMinutes = atoiEnv("PUBLICATION_SCAN_INTERVAL_MINUTES", 0)
	cfg.PublicationStageTimeoutMinutes = atoiEnv("PUBLICATION_STAGE_TIMEOUT_MINUTES", 0)
	cfg.ApplyPublicationDefaults()

	return cfg, nil
}

// atoiEnv reads an integer environment variable, returning def when unset,
// empty or unparseable (a warning goes to the process log via fmt).
func atoiEnv(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return def
	}
	return n
}

func loadFromFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, cfg)
}

// Save saves configuration to a file
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// IsDev returns true if running in development mode
func (c *Config) IsDev() bool {
	return c.Env == "development" || c.Env == "dev"
}

// IsProd returns true if running in production mode
func (c *Config) IsProd() bool {
	return c.Env == "production" || c.Env == "prod"
}
