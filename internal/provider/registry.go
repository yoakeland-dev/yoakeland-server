package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/fsutil"
)

type ProviderKind string

const (
	// ProviderKindAPIKey is a plain HTTP provider (anthropic/openai/openrouter)
	// authenticated with a manually registered API key.
	ProviderKindAPIKey ProviderKind = "api_key"
	// ProviderKindCLIClaude shells out to the user's own, already-authenticated
	// `claude` CLI instead of calling the API directly, so usage is billed to
	// their Claude subscription rather than metered API tokens.
	ProviderKindCLIClaude ProviderKind = "cli_claude"
	// ProviderKindCLICodex is the same idea for OpenAI's `codex` CLI /
	// ChatGPT subscription. Every other "cli_<kind>" (cli_cursor, cli_kimi,
	// cli_opencode, cli_gemini, cli_grok, cli_deepseek) is an ACP agent —
	// see CLIKindOf and cliSpecs.
	ProviderKindCLICodex ProviderKind = "cli_codex"
)

// CLIKindOf reports which CLI agent a "cli_<kind>" provider kind drives.
func CLIKindOf(k ProviderKind) (CLIKind, bool) {
	name, ok := strings.CutPrefix(string(k), "cli_")
	if !ok {
		return "", false
	}
	_, known := cliSpecs[CLIKind(name)]
	return CLIKind(name), known
}

// ProviderConfig is a named, persisted *credential*, not a model choice: one
// anthropic/openai/openrouter API key normally unlocks many models, so the
// model is picked per session (SetProviderData) via BuildProvider's model
// argument, not stored here. A client refers to a config by Name from then
// on; raw credentials are never re-sent once registered.
type ProviderConfig struct {
	Name string       `json:"name"`
	Kind ProviderKind `json:"kind"`

	// HTTPProvider selects the underlying HTTP backend for ProviderKindAPIKey:
	// "anthropic", "openai", or "openrouter".
	HTTPProvider string `json:"httpProvider,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`

	// AuthMethod is informational only ("manual" or "oauth"), for the
	// frontend to display how a credential was obtained.
	AuthMethod string `json:"authMethod,omitempty"`

	// FavoriteModels are model IDs the user starred for this provider (e.g.
	// to pin the handful they actually use out of a live list of hundreds).
	// Server-side so the picker doesn't reset per client/device. Stored as
	// opaque strings, not validated against ListModels — an upstream outage
	// or a temporarily-unavailable model shouldn't wipe a favorite.
	FavoriteModels []string `json:"favoriteModels,omitempty"`
}

type providerFileModel struct {
	Providers map[string]ProviderConfig `json:"providers"`
}

// encryptedProviderFile is the on-disk shape when a passphrase is
// configured: everything under it (salt, nonce, ciphertext) is safe to see,
// only the passphrase used to derive the AES key is secret.
type encryptedProviderFile struct {
	Encrypted  bool   `json:"encrypted"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// ProviderStore persists named provider configs as <dir>/providers.json.
// Providers are server-level state (they belong to whoever runs the
// server, not to any one project someone connects to work on), so dir is
// typically the server's state directory. Unlike
// sessions/permissions, this file can hold API keys, so it's written with
// 0600 rather than the usual 0644 — and, if a passphrase is given, encrypted
// at rest (AES-256-GCM, key derived via scrypt) rather than left plaintext,
// since file permissions alone don't survive a backup or a copied disk.
type ProviderStore struct {
	mu         sync.Mutex
	filePath   string
	passphrase string
	model      providerFileModel
}

// NewProviderStore opens (or creates) the provider store in dir. An empty
// passphrase stores providers.json as plaintext JSON, same as always. A
// non-empty passphrase encrypts it — including transparently encrypting an
// existing plaintext file the next time anything is registered/removed, so
// turning encryption on later needs no manual migration. A wrong passphrase
// against an already-encrypted file is a hard error: this never silently
// discards an unreadable registry.
func NewProviderStore(dir string, passphrase string) (*ProviderStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating provider store dir: %w", err)
	}

	s := &ProviderStore{
		filePath:   filepath.Join(dir, "providers.json"),
		passphrase: passphrase,
		model:      providerFileModel{Providers: make(map[string]ProviderConfig)},
	}

	if data, err := os.ReadFile(s.filePath); err == nil {
		if err := s.load(data); err != nil {
			return nil, err
		}
	}
	if s.model.Providers == nil {
		s.model.Providers = make(map[string]ProviderConfig)
	}
	return s, nil
}

func (s *ProviderStore) load(data []byte) error {
	var wrapper encryptedProviderFile
	if json.Unmarshal(data, &wrapper) == nil && wrapper.Encrypted {
		if s.passphrase == "" {
			return fmt.Errorf("providers.json is encrypted but no master-key passphrase was given")
		}
		salt, err := base64.StdEncoding.DecodeString(wrapper.Salt)
		if err != nil {
			return fmt.Errorf("malformed providers.json: bad salt: %w", err)
		}
		nonce, err := base64.StdEncoding.DecodeString(wrapper.Nonce)
		if err != nil {
			return fmt.Errorf("malformed providers.json: bad nonce: %w", err)
		}
		ciphertext, err := base64.StdEncoding.DecodeString(wrapper.Ciphertext)
		if err != nil {
			return fmt.Errorf("malformed providers.json: bad ciphertext: %w", err)
		}
		plaintext, err := decryptBytes(salt, nonce, ciphertext, s.passphrase)
		if err != nil {
			return err
		}
		return json.Unmarshal(plaintext, &s.model)
	}

	// Legacy/plaintext format. An unparseable file is set aside, not
	// overwritten by the next save: it may hold API keys still worth recovering.
	if err := json.Unmarshal(data, &s.model); err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", s.filePath, time.Now().Unix())
		if rerr := os.Rename(s.filePath, aside); rerr != nil {
			return fmt.Errorf("providers.json failed to parse and could not be set aside: %w", rerr)
		}
		log.Printf("provider store: providers.json failed to parse, moved to %s; starting empty: %v", aside, err)
		s.model = providerFileModel{Providers: make(map[string]ProviderConfig)}
	}
	return nil
}

func (s *ProviderStore) Register(cfg ProviderConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("provider name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// CLI-subprocess kinds hold no credential and there's only ever one
	// `claude`/`codex` CLI on the machine, so registering one of these kinds
	// again — even under a different Name — replaces the existing entry of
	// that same kind rather than leaving a stale duplicate slot behind. The
	// caller's chosen Name is still honored for lookup (set_provider,
	// providers.json), unlike a fixed internal slot name would be.
	if _, isCLI := CLIKindOf(cfg.Kind); isCLI {
		for name, existing := range s.model.Providers {
			if existing.Kind == cfg.Kind && name != cfg.Name {
				delete(s.model.Providers, name)
			}
		}
	}

	s.model.Providers[cfg.Name] = cfg
	return s.save()
}

func (s *ProviderStore) Get(name string) (ProviderConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.model.Providers[name]
	return cfg, ok
}

// SetFavoriteModels replaces the favorite-model list for a registered
// provider, leaving its credential and every other field untouched.
func (s *ProviderStore) SetFavoriteModels(name string, models []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.model.Providers[name]
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	cfg.FavoriteModels = models
	s.model.Providers[name] = cfg
	return s.save()
}

// List returns every registered provider with APIKey scrubbed, since it's
// meant to feed a frontend's provider picker, not to hand credentials back
// out over the wire.
func (s *ProviderStore) List() []ProviderConfig {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ProviderConfig, 0, len(s.model.Providers))
	for _, c := range s.model.Providers {
		c.APIKey = ""
		out = append(out, c)
	}
	return out
}

func (s *ProviderStore) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.model.Providers, name)
	return s.save()
}

func (s *ProviderStore) save() error {
	data, err := json.MarshalIndent(s.model, "", "  ")
	if err != nil {
		return err
	}

	out := data
	if s.passphrase != "" {
		salt, nonce, ciphertext, err := encryptBytes(data, s.passphrase)
		if err != nil {
			return fmt.Errorf("encrypting providers.json: %w", err)
		}
		out, err = json.MarshalIndent(encryptedProviderFile{
			Encrypted:  true,
			Salt:       base64.StdEncoding.EncodeToString(salt),
			Nonce:      base64.StdEncoding.EncodeToString(nonce),
			Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		}, "", "  ")
		if err != nil {
			return err
		}
	}

	return fsutil.WriteFileAtomic(s.filePath, out, 0600)
}

// BuildProvider constructs a live Provider from a stored credential plus the
// model a session picked for it (see SetProviderData/ListModels — one
// credential covers many models, so the model is never part of cfg).
// projectRoot is only used by the CLI-subprocess kinds, so the subprocess
// runs scoped to the right working directory.
func BuildProvider(cfg ProviderConfig, model string, projectRoot string) (Provider, error) {
	switch cfg.Kind {
	case ProviderKindAPIKey:
		return ResolveProvider(Config{
			ProviderName: cfg.HTTPProvider,
			Model:        model,
			APIKey:       cfg.APIKey,
			BaseURL:      cfg.BaseURL,
			NoEnv:        true,
		})
	default:
		if kind, ok := CLIKindOf(cfg.Kind); ok {
			return &CLISubprocessProvider{Kind: kind, Model: model, Dir: projectRoot}, nil
		}
		return nil, fmt.Errorf("unknown provider kind %q", cfg.Kind)
	}
}
