package staticdelivery

import (
	"bytes"
	"encoding/json"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestVaultAccountRevisionBinding(t *testing.T) {
	if _, e := NewVault(nil); e == nil {
		t.Fatal("missing key accepted")
	}
	v, e := NewVault(bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	a := primitive.NewObjectID()
	one, e := v.Seal(a, 1, []byte("private-token"))
	if e != nil {
		t.Fatal(e)
	}
	two, e := v.Seal(a, 1, []byte("private-token"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Equal(one.Nonce, two.Nonce) || bytes.Contains(one.Ciphertext, []byte("private-token")) {
		t.Fatal("nonce reuse/plaintext")
	}
	raw, e := v.Open(a, 1, one)
	if e != nil || string(raw) != "private-token" {
		t.Fatalf("%s %v", raw, e)
	}
	if _, e = v.Open(primitive.NewObjectID(), 1, one); e == nil {
		t.Fatal("cross-account decrypt")
	}
	if _, e = v.Open(a, 2, one); e == nil {
		t.Fatal("cross-revision decrypt")
	}
	changed := one
	changed.Ciphertext = append([]byte{}, one.Ciphertext...)
	changed.Ciphertext[0] ^= 1
	if _, e = v.Open(a, 1, changed); e == nil {
		t.Fatal("tamper accepted")
	}
	encoded, e := json.Marshal(one)
	if e != nil {
		t.Fatal(e)
	}
	if string(encoded) != "{}" {
		t.Fatalf("sealed data exposed: %s", encoded)
	}
	wrong, _ := NewVault(bytes.Repeat([]byte{8}, 32))
	if _, e = wrong.Open(a, 1, one); e == nil {
		t.Fatal("wrong key accepted")
	}
}
