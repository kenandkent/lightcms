package services

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// APIKeyService handles API key management
type APIKeyService struct {
	db *database.DB
	// encKey seals raw keys for the admin copy/reveal flow (AES-256-GCM).
	// Nil = copy unsupported (legacy behavior: show-once only).
	encKey []byte
}

// NewAPIKeyService creates a new API key service
func NewAPIKeyService(db *database.DB) *APIKeyService {
	return &APIKeyService{db: db}
}

// DeriveKeyEncryptionKey derives the 32-byte key-sealing key from the
// server session secret. Domain-separated so it cannot double as a session
// or CSRF key. Rotating the session secret permanently orphans sealed keys
// created under the old secret (reveal then fails closed — recreate them).
func DeriveKeyEncryptionKey(sessionSecret string) []byte {
	sum := sha256.Sum256([]byte("lightcms-apikey-reveal-v1:" + sessionSecret))
	return sum[:]
}

// SetEncryptionKey enables sealed storage + reveal for subsequently created
// keys. Pass nil to disable (show-once behavior, as before).
func (s *APIKeyService) SetEncryptionKey(key []byte) {
	if len(key) == 0 {
		s.encKey = nil
		return
	}
	cp := make([]byte, len(key))
	copy(cp, key)
	s.encKey = cp
}

// sealKey encrypts the raw key with AES-256-GCM (random nonce per key).
// Returns "" when no encryption key is configured.
func (s *APIKeyService) sealKey(rawKey string) (string, error) {
	if len(s.encKey) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(s.encKey)
	if err != nil {
		return "", fmt.Errorf("key cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("key GCM init: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("key nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(rawKey), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openKey decrypts a sealed key. Fails closed on any error (wrong secret
// after rotation, tampered row, unconfigured key).
func (s *APIKeyService) openKey(sealed string) (string, error) {
	if len(s.encKey) == 0 {
		return "", fmt.Errorf("key reveal is not configured on this server")
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("key envelope corrupt: %w", err)
	}
	block, err := aes.NewCipher(s.encKey)
	if err != nil {
		return "", fmt.Errorf("key cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("key GCM init: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("key envelope corrupt: too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("key decrypt failed (wrong server secret or tampered row): %w", err)
	}
	return string(plain), nil
}

// CreateAPIKeyForUser generates a new API key owned by a user, stores its hash, and returns the raw key (shown once)
func (s *APIKeyService) CreateAPIKeyForUser(ctx context.Context, name, description string, userID *primitive.ObjectID) (string, *models.APIKey, error) {
	return s.CreateScopedAPIKey(ctx, name, description, userID, nil, false)
}

// CreateScopedAPIKey creates an API key with an optional permission
// allowlist (scopes) and sandbox-only restriction — the governance surface
// for keys handed to AI agents.
func (s *APIKeyService) CreateScopedAPIKey(ctx context.Context, name, description string, userID *primitive.ObjectID, scopes []string, sandboxOnly bool) (string, *models.APIKey, error) {
	if name == "" {
		return "", nil, fmt.Errorf("name is required")
	}

	// Generate 32 random bytes → 64 hex chars, but we use 32 hex chars for the key
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, fmt.Errorf("failed to generate random key: %w", err)
	}
	rawKey := "lc_" + hex.EncodeToString(randomBytes)

	// Hash the key with SHA-256
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	// Store prefix for identification (lc_ + first 8 hex chars)
	prefix := rawKey[:11]

	now := time.Now()
	apiKey := &models.APIKey{
		Name:        name,
		Description: description,
		Prefix:      prefix,
		KeyHash:     keyHash,
		UserID:      userID,
		Scopes:      scopes,
		SandboxOnly: sandboxOnly,
		CreatedAt:   now,
	}
	// Seal a recoverable copy when configured; failures fail the create
	// loudly rather than silently producing an uncopyable key.
	if len(s.encKey) > 0 {
		sealed, serr := s.sealKey(rawKey)
		if serr != nil {
			return "", nil, serr
		}
		apiKey.KeyCiphertext = sealed
	}

	id, err := s.db.InsertOne(ctx, "api_keys", apiKey)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}
	apiKey.ID = id

	return rawKey, apiKey, nil
}

// CreateAPIKey generates a new API key without user ownership (for system/legacy use)
func (s *APIKeyService) CreateAPIKey(ctx context.Context, name, description string) (string, *models.APIKey, error) {
	return s.CreateAPIKeyForUser(ctx, name, description, nil)
}

// ListAPIKeys returns all API keys (without raw key values)
func (s *APIKeyService) ListAPIKeys(ctx context.Context) ([]models.APIKey, error) {
	cursor, err := s.db.FindMany(ctx, "api_keys", bson.M{},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}
	defer cursor.Close(ctx)

	var keys []models.APIKey
	if err := cursor.All(ctx, &keys); err != nil {
		return nil, fmt.Errorf("failed to decode API keys: %w", err)
	}
	return keys, nil
}

// ListAPIKeysForUser returns API keys owned by a specific user
func (s *APIKeyService) ListAPIKeysForUser(ctx context.Context, userID primitive.ObjectID) ([]models.APIKey, error) {
	cursor, err := s.db.FindMany(ctx, "api_keys", bson.M{"user_id": userID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}
	defer cursor.Close(ctx)

	var keys []models.APIKey
	if err := cursor.All(ctx, &keys); err != nil {
		return nil, fmt.Errorf("failed to decode API keys: %w", err)
	}
	return keys, nil
}

// DeleteAPIKey deletes an API key by ID. Admins may delete any key.
func (s *APIKeyService) DeleteAPIKey(ctx context.Context, id primitive.ObjectID) error {
	return s.db.DeleteOne(ctx, "api_keys", bson.M{"_id": id})
}

// DeleteAPIKeyForUser deletes an API key only if it is owned by the given user.
// Returns an error if the key does not exist or belongs to a different user.
func (s *APIKeyService) DeleteAPIKeyForUser(ctx context.Context, id primitive.ObjectID, ownerID primitive.ObjectID) error {
	return s.db.DeleteOne(ctx, "api_keys", bson.M{"_id": id, "user_id": ownerID})
}

// Reveal errors: callers map these to HTTP statuses (404/403/410).
var (
	// ErrAPIKeyNotFound is returned when no key matches the ID.
	ErrAPIKeyNotFound = errors.New("api key not found")
	// ErrAPIKeyForbidden is returned when the requester owns neither the
	// key nor an admin role.
	ErrAPIKeyForbidden = errors.New("not allowed to reveal this api key")
	// ErrAPIKeyNotRevealable is returned for legacy keys created before
	// copy support (no sealed copy stored) — they must be recreated.
	ErrAPIKeyNotRevealable = errors.New("key created before copy support; delete and recreate it to enable copying")
)

// GetAPIKey loads one key record by ID (never includes the raw key).
func (s *APIKeyService) GetAPIKey(ctx context.Context, id primitive.ObjectID) (*models.APIKey, error) {
	var key models.APIKey
	if err := s.db.FindOne(ctx, "api_keys", bson.M{"_id": id}, &key); err != nil {
		return nil, ErrAPIKeyNotFound
	}
	return &key, nil
}

// RevealAPIKey decrypts and returns the raw key for the admin copy flow.
// Only the owning user or an admin may reveal; every other caller gets
// ErrAPIKeyForbidden. Legacy keys without a sealed copy get
// ErrAPIKeyNotRevealable. Callers must audit-log successful reveals.
func (s *APIKeyService) RevealAPIKey(ctx context.Context, id primitive.ObjectID, requestingUserID string, isAdmin bool) (string, error) {
	key, err := s.GetAPIKey(ctx, id)
	if err != nil {
		return "", err
	}
	owned := key.UserID != nil && requestingUserID != "" && key.UserID.Hex() == requestingUserID
	if !owned && !isAdmin {
		return "", ErrAPIKeyForbidden
	}
	if key.KeyCiphertext == "" {
		return "", ErrAPIKeyNotRevealable
	}
	return s.openKey(key.KeyCiphertext)
}

// ValidateAPIKey checks a raw API key, returns the key record if valid, and updates last_used_at
func (s *APIKeyService) ValidateAPIKey(ctx context.Context, rawKey string) (*models.APIKey, error) {
	if len(rawKey) < 4 || rawKey[:3] != "lc_" {
		return nil, fmt.Errorf("invalid API key format")
	}

	// Hash the provided key
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	// Look up by hash
	var apiKey models.APIKey
	err := s.db.FindOne(ctx, "api_keys", bson.M{"key_hash": keyHash}, &apiKey)
	if err != nil {
		return nil, fmt.Errorf("invalid API key")
	}

	// Update last_used_at (fire-and-forget)
	now := time.Now()
	_ = s.db.UpdateOne(ctx, "api_keys", bson.M{"_id": apiKey.ID}, bson.M{"$set": bson.M{"last_used_at": now}})
	apiKey.LastUsedAt = &now

	return &apiKey, nil
}
