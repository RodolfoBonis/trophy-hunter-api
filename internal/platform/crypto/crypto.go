// Package crypto cifra credenciais de integração em repouso com AES-256-GCM.
//
// Formato do blob: version(uint32 BE) || nonce(12 bytes) || ciphertext+tag(GCM).
//   - version prefixa o blob para permitir rotação de chave sem re-cifrar tudo de uma vez: o
//     Cipher guarda as chaves por versão e escolhe a certa na decifragem.
//   - O nonce é aleatório (crypto/rand) por operação — NUNCA reutilizado para a mesma chave.
//   - A AAD (dados adicionais autenticados) é a plataforma da integração ("steam"/"psn"): liga o
//     ciphertext ao seu contexto, impedindo que um blob seja movido entre linhas/plataformas.
//
// A chave mestra NUNCA é logada e não possui String()/Format que a exponha.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrDecrypt é devolvido quando a decifragem falha (chave desconhecida, blob adulterado, AAD
// incorreta ou formato inválido). NÃO distingue as causas para o chamador (evita oráculo).
var ErrDecrypt = errors.New("falha ao decifrar credencial")

const (
	keyLen     = 32 // AES-256
	versionLen = 4
	nonceLen   = 12 // padrão GCM
)

// Cipher cifra/decifra credenciais. É imutável após a construção e seguro para uso concorrente.
type Cipher struct {
	primaryVersion int
	gcms           map[int]cipher.AEAD
}

// New constrói o Cipher com uma chave primária (a usada para cifrar) de 32 bytes.
func New(primaryVersion int, primaryKey []byte) (*Cipher, error) {
	c := &Cipher{primaryVersion: primaryVersion, gcms: make(map[int]cipher.AEAD)}
	if err := c.AddKey(primaryVersion, primaryKey); err != nil {
		return nil, err
	}
	return c, nil
}

// AddKey registra uma chave antiga por versão para suportar decifragem durante a rotação.
func (c *Cipher) AddKey(version int, key []byte) error {
	if len(key) != keyLen {
		return fmt.Errorf("chave da versão %d deve ter %d bytes, tem %d", version, keyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("gcm: %w", err)
	}
	c.gcms[version] = gcm
	return nil
}

// Encrypt cifra plaintext ligando-o à plataforma via AAD. Devolve o blob versionado.
func (c *Cipher) Encrypt(plaintext []byte, platform string) ([]byte, error) {
	gcm := c.gcms[c.primaryVersion]
	if gcm == nil {
		return nil, fmt.Errorf("sem chave para a versão primária %d", c.primaryVersion)
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("gerar nonce: %w", err)
	}
	out := make([]byte, versionLen+nonceLen)
	binary.BigEndian.PutUint32(out[:versionLen], uint32(c.primaryVersion)) //nolint:gosec // versão não-negativa
	copy(out[versionLen:], nonce)
	return gcm.Seal(out, nonce, plaintext, []byte(platform)), nil
}

// Decrypt decifra um blob produzido por Encrypt, validando a AAD (platform). Qualquer falha
// devolve ErrDecrypt, sem detalhar a causa.
func (c *Cipher) Decrypt(blob []byte, platform string) ([]byte, error) {
	if len(blob) < versionLen+nonceLen {
		return nil, ErrDecrypt
	}
	version := int(binary.BigEndian.Uint32(blob[:versionLen]))
	gcm := c.gcms[version]
	if gcm == nil {
		return nil, ErrDecrypt
	}
	nonce := blob[versionLen : versionLen+nonceLen]
	ciphertext := blob[versionLen+nonceLen:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(platform))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// Version devolve a versão de chave primária (para persistir em integrations.key_version).
func (c *Cipher) Version() int { return c.primaryVersion }
