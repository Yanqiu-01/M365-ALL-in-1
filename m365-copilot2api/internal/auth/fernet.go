package auth

// Fernet compatibility for the encrypted store written by the Python desktop
// gateway. The wire format follows fernet-spec: a version byte, an 8-byte
// Unix timestamp, a random 16-byte IV, AES-128-CBC ciphertext, and an
// HMAC-SHA256 over everything before the HMAC. The 32-byte URL-safe base64
// key is split into a 16-byte signing key followed by a 16-byte AES key.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const encryptedStoreMagic = "m365-token-store"

type encryptedStoreEnvelope struct {
	Magic      string `json:"magic"`
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Ciphertext string `json:"ciphertext"`
}

func storeKeyPath(storePath string) string {
	if p := strings.TrimSpace(os.Getenv("M365_STORE_KEY_FILE")); p != "" {
		return p
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "m365-store.key")
	}
	return filepath.Join(filepath.Dir(storePath), "m365-store.key")
}

func loadFernetKey(storePath string) ([]byte, error) {
	path := storeKeyPath(storePath)
	material, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read store key %s: %w", path, err)
	}
	encoded := strings.TrimSpace(string(material))
	key, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		// Fernet keys are normally padded, but accepting the raw URL-safe form
		// makes the reader tolerant of equivalent key-file encodings.
		key, err = base64.RawURLEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("store key %s is not a 32-byte Fernet key", path)
	}
	return key, nil
}

func decryptEncryptedStore(raw []byte, storePath string) ([]byte, []byte, error) {
	var envelope encryptedStoreEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, fmt.Errorf("decode encrypted store envelope: %w", err)
	}
	if envelope.Magic != encryptedStoreMagic {
		return nil, nil, errors.New("not an encrypted m365 token store")
	}
	if envelope.Ciphertext == "" {
		return nil, nil, errors.New("encrypted store has no ciphertext")
	}
	key, err := loadFernetKey(storePath)
	if err != nil {
		return nil, nil, err
	}
	token, err := base64.URLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		token, err = base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("decode encrypted store ciphertext: %w", err)
	}
	plain, err := fernetDecrypt(token, key)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt encrypted store: %w", err)
	}
	return plain, key, nil
}

func encryptStoreBody(body []byte, storePath string, key []byte) ([]byte, error) {
	if len(key) != 32 {
		loaded, err := loadFernetKey(storePath)
		if err != nil {
			return nil, err
		}
		key = loaded
	}
	token, err := fernetEncrypt(body, key)
	if err != nil {
		return nil, err
	}
	envelope := encryptedStoreEnvelope{
		Magic:      encryptedStoreMagic,
		Version:    1,
		KDF:        "fernet",
		Ciphertext: base64.URLEncoding.EncodeToString(token),
	}
	return json.MarshalIndent(envelope, "", "  ")
}

func fernetDecrypt(token, key []byte) ([]byte, error) {
	if len(key) != 32 || len(token) < 1+8+16+aes.BlockSize+32 {
		return nil, errors.New("invalid Fernet token")
	}
	if token[0] != 0x80 {
		return nil, errors.New("unsupported Fernet version")
	}
	body, signature := token[:len(token)-sha256.Size], token[len(token)-sha256.Size:]
	mac := hmac.New(sha256.New, key[:16])
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, errors.New("Fernet HMAC verification failed")
	}
	block, err := aes.NewCipher(key[16:])
	if err != nil {
		return nil, err
	}
	ciphertext := token[1+8+16 : len(token)-sha256.Size]
	if len(ciphertext) == 0 || len(ciphertext)%block.BlockSize() != 0 {
		return nil, errors.New("invalid Fernet ciphertext length")
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, token[1+8:1+8+16]).CryptBlocks(plain, ciphertext)
	return unpadPKCS7(plain, block.BlockSize())
}

func fernetEncrypt(plain, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("invalid Fernet key")
	}
	block, err := aes.NewCipher(key[16:])
	if err != nil {
		return nil, err
	}
	padded := padPKCS7(plain, block.BlockSize())
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	token := make([]byte, 1+8+len(iv)+len(ciphertext))
	token[0] = 0x80
	binary.BigEndian.PutUint64(token[1:9], uint64(time.Now().Unix()))
	copy(token[9:25], iv)
	copy(token[25:], ciphertext)
	mac := hmac.New(sha256.New, key[:16])
	_, _ = mac.Write(token)
	return append(token, mac.Sum(nil)...), nil
}

func padPKCS7(in []byte, size int) []byte {
	padding := size - len(in)%size
	out := make([]byte, len(in)+padding)
	copy(out, in)
	for i := len(in); i < len(out); i++ {
		out[i] = byte(padding)
	}
	return out
}

func unpadPKCS7(in []byte, size int) ([]byte, error) {
	if len(in) == 0 || len(in)%size != 0 {
		return nil, errors.New("invalid PKCS7 padding length")
	}
	padding := int(in[len(in)-1])
	if padding == 0 || padding > size || padding > len(in) {
		return nil, errors.New("invalid PKCS7 padding")
	}
	for _, b := range in[len(in)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid PKCS7 padding")
		}
	}
	return in[:len(in)-padding], nil
}
