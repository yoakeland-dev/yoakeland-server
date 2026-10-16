package mcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"yoakeland/internal/secretfile"
)

// sealedFile is the envelope providers.json uses. With a master key set, mcp.json
// and mcp-auth.json are written in it too. A plaintext file from before the key
// was set is read as it is, and is sealed the next time it is written.
type sealedFile struct {
	Encrypted  bool   `json:"encrypted"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// openFile returns the JSON in data, decrypting it when it is sealed.
func openFile(data []byte, key string) ([]byte, error) {
	var f sealedFile
	if json.Unmarshal(data, &f) != nil || !f.Encrypted {
		return data, nil
	}
	if key == "" {
		return nil, errors.New("it is encrypted, but no master key is set")
	}
	salt, err := base64.StdEncoding.DecodeString(f.Salt)
	if err != nil {
		return nil, fmt.Errorf("malformed: bad salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(f.Nonce)
	if err != nil {
		return nil, fmt.Errorf("malformed: bad nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(f.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("malformed: bad ciphertext: %w", err)
	}
	return secretfile.Decrypt(salt, nonce, ciphertext, key)
}

// sealFile returns what to write for the JSON in data: sealed when a key is set.
func sealFile(data []byte, key string) ([]byte, error) {
	if key == "" {
		return data, nil
	}
	salt, nonce, ciphertext, err := secretfile.Encrypt(data, key)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(sealedFile{
		Encrypted:  true,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, "", "  ")
}
