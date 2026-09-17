package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"yoakeland/internal/provider"
)

func TestProviderStore_EncryptsAtRestWhenPassphraseGiven(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "correct horse battery staple")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{
		Name: "secret-provider", Kind: provider.ProviderKindAPIKey, APIKey: "sk-super-secret-value",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(tempDir, "providers.json"))
	if err != nil {
		t.Fatalf("reading providers.json: %v", err)
	}

	if strings.Contains(string(raw), "sk-super-secret-value") {
		t.Fatalf("the API key must not appear in plaintext on disk when a passphrase is set, got: %s", raw)
	}

	var wrapper struct {
		Encrypted  bool   `json:"encrypted"`
		Salt       string `json:"salt"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("providers.json isn't valid JSON: %v", err)
	}
	if !wrapper.Encrypted || wrapper.Salt == "" || wrapper.Nonce == "" || wrapper.Ciphertext == "" {
		t.Fatalf("expected a fully populated encrypted wrapper, got: %+v", wrapper)
	}
}

func TestProviderStore_DecryptsWithCorrectPassphrase(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "my-passphrase")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, APIKey: "the-real-key"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reopened, err := provider.NewProviderStore(tempDir, "my-passphrase")
	if err != nil {
		t.Fatalf("re-opening with the correct passphrase: %v", err)
	}
	cfg, ok := reopened.Get("p")
	if !ok || cfg.APIKey != "the-real-key" {
		t.Fatalf("expected the decrypted config to round-trip, got %+v, ok=%v", cfg, ok)
	}
}

func TestProviderStore_WrongPassphraseFailsLoudly(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "correct-passphrase")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, APIKey: "secret"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := provider.NewProviderStore(tempDir, "wrong-passphrase"); err == nil {
		t.Fatal("expected opening an encrypted store with the wrong passphrase to fail, not silently discard it")
	}
}

func TestProviderStore_EncryptedFileWithNoPassphraseFailsLoudly(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "some-passphrase")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, APIKey: "secret"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := provider.NewProviderStore(tempDir, ""); err == nil {
		t.Fatal("expected opening an encrypted store with no passphrase at all to fail")
	}
}

// TestProviderStore_TurningOnEncryptionMigratesExistingPlaintext guards the
// upgrade path: a store that starts plaintext and later gets a passphrase
// must transparently re-encrypt on its next write, with no manual migration
// step and no data loss.
func TestProviderStore_TurningOnEncryptionMigratesExistingPlaintext(t *testing.T) {
	tempDir := t.TempDir()

	plain, err := provider.NewProviderStore(tempDir, "")
	if err != nil {
		t.Fatalf("NewProviderStore (plaintext): %v", err)
	}
	if err := plain.Register(provider.ProviderConfig{Name: "old-entry", Kind: provider.ProviderKindAPIKey, APIKey: "old-key"}); err != nil {
		t.Fatalf("Register (plaintext): %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(tempDir, "providers.json"))
	if strings.Contains(string(raw), `"encrypted"`) {
		t.Fatalf("expected a plain (unwrapped) file before encryption is turned on, got: %s", raw)
	}

	// Reopen with a passphrase now configured: must still read the old
	// plaintext entry correctly...
	encrypted, err := provider.NewProviderStore(tempDir, "new-passphrase")
	if err != nil {
		t.Fatalf("NewProviderStore (turning on encryption): %v", err)
	}
	cfg, ok := encrypted.Get("old-entry")
	if !ok || cfg.APIKey != "old-key" {
		t.Fatalf("expected the pre-existing plaintext entry to survive, got %+v, ok=%v", cfg, ok)
	}

	// ...and the next write must actually encrypt the file.
	if err := encrypted.Register(provider.ProviderConfig{Name: "new-entry", Kind: provider.ProviderKindAPIKey, APIKey: "new-key"}); err != nil {
		t.Fatalf("Register (post-migration): %v", err)
	}
	raw, _ = os.ReadFile(filepath.Join(tempDir, "providers.json"))
	if strings.Contains(string(raw), "old-key") || strings.Contains(string(raw), "new-key") {
		t.Fatalf("expected the file to be encrypted after the first write with a passphrase set, got: %s", raw)
	}

	reopened, err := provider.NewProviderStore(tempDir, "new-passphrase")
	if err != nil {
		t.Fatalf("re-opening after migration: %v", err)
	}
	if cfg, ok := reopened.Get("old-entry"); !ok || cfg.APIKey != "old-key" {
		t.Fatalf("old entry lost across migration: %+v, ok=%v", cfg, ok)
	}
	if cfg, ok := reopened.Get("new-entry"); !ok || cfg.APIKey != "new-key" {
		t.Fatalf("new entry lost across migration: %+v, ok=%v", cfg, ok)
	}
}
