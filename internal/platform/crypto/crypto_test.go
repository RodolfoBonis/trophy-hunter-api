package crypto_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/crypto"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

func TestRoundtrip(t *testing.T) {
	c, err := crypto.New(1, newKey(t))
	require.NoError(t, err)

	plaintext := []byte("super-secret-steam-api-key-ABC123")
	blob, err := c.Encrypt(plaintext, "steam")
	require.NoError(t, err)
	require.NotContains(t, string(blob), "super-secret", "o segredo não deve aparecer em claro no blob")

	got, err := c.Decrypt(blob, "steam")
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestWrongAADFails(t *testing.T) {
	c, err := crypto.New(1, newKey(t))
	require.NoError(t, err)

	blob, err := c.Encrypt([]byte("x"), "steam")
	require.NoError(t, err)

	_, err = c.Decrypt(blob, "psn")
	require.ErrorIs(t, err, crypto.ErrDecrypt, "AAD diferente deve falhar")
}

func TestTamperFails(t *testing.T) {
	c, err := crypto.New(1, newKey(t))
	require.NoError(t, err)

	blob, err := c.Encrypt([]byte("hello world"), "steam")
	require.NoError(t, err)

	// Adultera o último byte (tag GCM).
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 0xFF
	_, err = c.Decrypt(tampered, "steam")
	require.ErrorIs(t, err, crypto.ErrDecrypt)

	// Adultera o nonce.
	tampered2 := bytes.Clone(blob)
	tampered2[5] ^= 0xFF
	_, err = c.Decrypt(tampered2, "steam")
	require.ErrorIs(t, err, crypto.ErrDecrypt)
}

func TestUnknownKeyVersionFails(t *testing.T) {
	c1, err := crypto.New(1, newKey(t))
	require.NoError(t, err)
	blob, err := c1.Encrypt([]byte("x"), "steam")
	require.NoError(t, err)

	// Outro Cipher só conhece a versão 2 → não consegue decifrar o blob da versão 1.
	c2, err := crypto.New(2, newKey(t))
	require.NoError(t, err)
	_, err = c2.Decrypt(blob, "steam")
	require.ErrorIs(t, err, crypto.ErrDecrypt)
}

func TestRotationDecryptsOldBlob(t *testing.T) {
	oldKey := newKey(t)
	c1, err := crypto.New(1, oldKey)
	require.NoError(t, err)
	oldBlob, err := c1.Encrypt([]byte("legacy"), "steam")
	require.NoError(t, err)

	// Novo Cipher com chave primária v2, mas com a v1 registrada para decifrar blobs antigos.
	c2, err := crypto.New(2, newKey(t))
	require.NoError(t, err)
	require.NoError(t, c2.AddKey(1, oldKey))

	got, err := c2.Decrypt(oldBlob, "steam")
	require.NoError(t, err)
	require.Equal(t, []byte("legacy"), got)
	require.Equal(t, 2, c2.Version())
}

func TestNonceIsRandom(t *testing.T) {
	c, err := crypto.New(1, newKey(t))
	require.NoError(t, err)
	a, err := c.Encrypt([]byte("same"), "steam")
	require.NoError(t, err)
	b, err := c.Encrypt([]byte("same"), "steam")
	require.NoError(t, err)
	require.NotEqual(t, a, b, "nonce aleatório deve produzir blobs distintos para o mesmo plaintext")
}

func TestNewRejectsBadKeyLen(t *testing.T) {
	_, err := crypto.New(1, []byte("short"))
	require.Error(t, err)
}
