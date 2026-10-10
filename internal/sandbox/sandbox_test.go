package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/sandbox"
)

func TestSandbox_ExecutionAndTimeout(t *testing.T) {
	sb := sandbox.NewSandbox(t.TempDir(), 1*time.Second)

	// Quick execution
	res, err := sb.Execute(context.Background(), []string{"echo", "sandbox active"}, nil)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}

	// Timeout enforcement
	ctx := context.Background()
	_, err = sb.Execute(ctx, []string{"sleep", "2"}, nil)
	if err == nil {
		t.Fatalf("expected timeout error for 2-second sleep in 1-second sandbox")
	}
}

// The rc-script dir must be private: another local user who could write it
// would get their script sourced in every terminal.
func TestShellIntegrationDirIsPrivate(t *testing.T) {
	dir := sandbox.ShellIntegrationDir()
	if dir == "" {
		t.Skip("no temp dir")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	if sandbox.ShellIntegrationDir() != dir {
		t.Fatal("dir changed between calls")
	}
}

// Sandboxed commands may write the project but not .git/config or hooks,
// which git runs as code on the host.
func TestSandbox_GitConfigReadOnly(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "modules", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "modules", "sub", "config"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sb := sandbox.NewSandbox(root, 10*time.Second)
	if sb.Mode() != sandbox.ModeBubblewrap {
		t.Skip("needs bubblewrap")
	}
	for _, argv := range [][]string{
		{"sh", "-c", "echo '[core]' >> .git/modules/sub/config"},
		{"git", "config", "core.fsmonitor", "touch x"},
		{"sh", "-c", "echo '[core]' >> .git/config"},
		{"sh", "-c", "echo x > .git/hooks/pre-commit"},
	} {
		res, err := sb.Execute(context.Background(), argv, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode == 0 {
			t.Errorf("%v succeeded", argv)
		}
	}
	res, err := sb.Execute(context.Background(), []string{"sh", "-c", "echo ok > file.txt"}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("project write failed: %v %+v", err, res)
	}
}

// A sandboxed command sees an empty folder where yoakeland's state is.
func TestSandbox_HidesStateDir(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, ".yoake")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "server.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := normalizer.Hidden
	normalizer.Hidden = []string{state}
	defer func() { normalizer.Hidden = old }()
	sb := sandbox.NewSandbox(root, 10*time.Second)
	if sb.Mode() != sandbox.ModeBubblewrap {
		t.Skip("needs bubblewrap")
	}
	res, err := sb.Execute(context.Background(), []string{"cat", filepath.Join(state, "server.json")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || strings.Contains(res.Stdout, "secret") {
		t.Fatalf("state readable in the sandbox: %+v", res)
	}
}

// .git is a mountpoint in the sandbox, so it can't be renamed away and
// recreated without the read-only config/hooks binds; git still works.
func TestSandbox_GitDirCannotBeRenamed(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	sb := sandbox.NewSandbox(root, 10*time.Second)
	if sb.Mode() != sandbox.ModeBubblewrap {
		t.Skip("needs bubblewrap")
	}
	res, err := sb.Execute(context.Background(), []string{"mv", ".git", ".git-old"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("mv .git succeeded in the sandbox")
	}
	res, err = sb.Execute(context.Background(), []string{"sh", "-c", "echo x > f && git add f"}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("git add failed: %v %+v", err, res)
	}
}
