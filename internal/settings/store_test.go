package settings_test

import (
	"testing"
	"yoakeland/internal/settings"
)

func TestStore_DefaultsToWebToolsEnabled(t *testing.T) {
	tempDir := t.TempDir()
	store := settings.NewStore(tempDir)

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.WebToolsEnabled {
		t.Fatal("expected WebToolsEnabled to default to true when no settings.json exists yet")
	}
}

func TestStore_SaveAndLoadRoundTrips(t *testing.T) {
	tempDir := t.TempDir()
	store := settings.NewStore(tempDir)

	if err := store.Save(settings.ProjectSettings{Version: 1, WebToolsEnabled: false}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.WebToolsEnabled {
		t.Fatal("expected the saved WebToolsEnabled=false to round-trip")
	}

	// A second store rooted at the same directory (simulating a server
	// restart) must read back the same persisted value.
	reopened := settings.NewStore(tempDir)
	got2, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load (reopened): %v", err)
	}
	if got2.WebToolsEnabled {
		t.Fatal("expected the persisted setting to survive across Store instances")
	}
}
