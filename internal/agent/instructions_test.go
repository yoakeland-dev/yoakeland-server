package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/normalizer"
)

// AGENTS.md reaches the provider, so a symlinked one must not carry a file
// from outside the project or from yoakeland's own state into the prompt.
func TestProjectInstructions_OnlyFromInsideProject(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	state := filepath.Join(root, "state")
	_ = os.MkdirAll(state, 0o700)
	_ = os.WriteFile(filepath.Join(state, "server.json"), []byte("STATE-SECRET"), 0o600)
	_ = os.WriteFile(filepath.Join(outside, "id_ed25519"), []byte("OUTSIDE-SECRET"), 0o600)
	old := normalizer.Hidden
	normalizer.Hidden = []string{state}
	defer func() { normalizer.Hidden = old }()

	link := filepath.Join(root, "AGENTS.md")
	for _, target := range []string{filepath.Join(outside, "id_ed25519"), filepath.Join(state, "server.json")} {
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if got := projectInstructions(root); strings.Contains(got, "SECRET") {
			t.Fatalf("AGENTS.md -> %s leaked: %q", target, got)
		}
	}

	_ = os.Remove(link)
	_ = os.WriteFile(link, []byte("run go test ./..."), 0o644)
	if got := projectInstructions(root); !strings.Contains(got, "run go test ./...") {
		t.Fatalf("ordinary AGENTS.md not included: %q", got)
	}
}
