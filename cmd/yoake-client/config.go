package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ClientConfig persists the last-used connection settings so a device only
// needs to pass -connect/-token/-ca once, not on every run.
type ClientConfig struct {
	Connect string `json:"connect,omitempty"`
	Token   string `json:"token,omitempty"`
	CA      string `json:"ca,omitempty"`
	Name    string `json:"name,omitempty"`
	Session string `json:"session,omitempty"`
}

func clientConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".yoake", "client.json"), nil
}

// loadClientConfig returns a zero-value config (not an error) if none was
// ever saved.
func loadClientConfig() ClientConfig {
	var cfg ClientConfig
	path, err := clientConfigPath()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveClientConfig(cfg ClientConfig) error {
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
