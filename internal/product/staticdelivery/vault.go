package staticdelivery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Sealed struct {
	Ciphertext []byte `json:"-"`
	Nonce      []byte `json:"-"`
	KeyID      string `json:"-"`
}
type Vault struct {
	gcm   cipher.AEAD
	keyID string
}

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, failure(CodeCredentialUnavailable, "static delivery master key must contain 32 bytes", nil)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, failure(CodeCredentialUnavailable, "cannot initialize credential encryption", nil)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(key)
	return &Vault{gcm: gcm, keyID: hex.EncodeToString(hash[:8])}, nil
}
func credentialAAD(account primitive.ObjectID, revision int64) []byte {
	return []byte(fmt.Sprintf("static-delivery:credential:v1:%s:%d", account.Hex(), revision))
}
func (v *Vault) Seal(account primitive.ObjectID, revision int64, raw []byte) (Sealed, error) {
	if v == nil || v.gcm == nil {
		return Sealed{}, failure(CodeCredentialUnavailable, "credential encryption unavailable", nil)
	}
	if account.IsZero() || revision < 1 || len(raw) == 0 || len(raw) > 8192 {
		return Sealed{}, failure(CodeInvalidRequest, "account, revision and nonempty token required", nil)
	}
	nonce := make([]byte, v.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{Ciphertext: v.gcm.Seal(nil, nonce, raw, credentialAAD(account, revision)), Nonce: nonce, KeyID: v.keyID}, nil
}
func (v *Vault) Open(account primitive.ObjectID, revision int64, s Sealed) ([]byte, error) {
	if v == nil || v.gcm == nil || s.KeyID != v.keyID || len(s.Nonce) != v.gcm.NonceSize() {
		return nil, failure(CodeCredentialUnavailable, "credential key unavailable", nil)
	}
	raw, err := v.gcm.Open(nil, s.Nonce, s.Ciphertext, credentialAAD(account, revision))
	if err != nil {
		return nil, failure(CodeCredentialUnavailable, "credential cannot be decrypted", nil)
	}
	return raw, nil
}
