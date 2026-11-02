package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"yoakeland/internal/projectstore"
	"yoakeland/internal/sockpath"
)

// What belongs to the user. Everything else stays behind: tls/ and server.json hold this server's
// identity and secrets (tokens, keys), which a copy must not share; logs, audit and caches are not data.
var importedState = []string{"projects", "chats", "uploads", "snapshots", "providers.json", "mcp.json", "skills.json", "schedules.json", "projects.json", "spend.json", "usage.json"}

// server.json settings carried over: the name and icon stay, but a copy is a separate server, so no serverId, tokens or remote listener.
var importedSettings = []string{"identity", "icon", "autoGenerateTitle", "modelRoles", "notify", "runtime", "defaultProvider"}

// Present in the target while a copy runs, so one cut short is redone rather than mistaken for a state dir.
const importMarker = ".copying"

// needsImport: the state dir is new, or holds only a copy that never finished.
func needsImport(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, importMarker))
	return err == nil
}

// importState copies another server's state dir into `to`. A chat's scratch root lives inside the
// state dir (<state>/chats/<id>), so its history is filed under the hash of that path; moving the
// state changes the path, so those are re-filed and the paths inside the records rewritten.
// Best done with the source server stopped: a file caught mid-write is copied torn.
func importState(from, to string) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(to, importMarker), nil, 0o600); err != nil {
		return err
	}
	for _, n := range importedState {
		src := filepath.Join(from, n)
		st, err := os.Stat(src)
		if err != nil {
			continue
		}
		if st.IsDir() {
			err = os.CopyFS(filepath.Join(to, n), os.DirFS(src))
		} else {
			err = copyStateFile(src, filepath.Join(to, n))
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", n, err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(from, "server.json")); err == nil {
		var old, kept = map[string]json.RawMessage{}, map[string]json.RawMessage{}
		if json.Unmarshal(raw, &old) == nil {
			for _, k := range importedSettings {
				if v, ok := old[k]; ok && string(v) != "null" {
					kept[k] = v
				}
			}
			out, _ := json.MarshalIndent(kept, "", "  ")
			if err := os.WriteFile(filepath.Join(to, "server.json"), out, 0o600); err != nil {
				return err
			}
		}
	}
	if ids, err := os.ReadDir(filepath.Join(from, "chats")); err == nil {
		for _, id := range ids {
			stale := projectstore.DataDir(to, filepath.Join(from, "chats", id.Name()))
			if _, err := os.Stat(stale); err == nil {
				if err := os.Rename(stale, projectstore.DataDir(to, filepath.Join(to, "chats", id.Name()))); err != nil {
					return err
				}
			}
		}
	}
	if err := rewriteStatePaths(filepath.Join(to, "projects"), from, to); err != nil {
		return err
	}
	return os.Remove(filepath.Join(to, importMarker))
}

func copyStateFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func rewriteStatePaths(dir, from, to string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == dir {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), from+"/") {
			return err
		}
		return os.WriteFile(p, []byte(strings.ReplaceAll(string(b), from+"/", to+"/")), 0o600)
	})
}

// offerImport copies -import-from when given, with no questions. Otherwise, when `offer` (the state dir
// was left to the default), it asks on a first run at a terminal whether to start from stable's data.
// A daemon the app starts passes -state-dir and has no terminal, so it never asks.
func offerImport(stateDir, importFrom string, offer bool) {
	if importFrom != "" {
		if !needsImport(stateDir) {
			fmt.Fprintln(os.Stderr, "yoakeland: -import-from only applies to a new state dir; ignoring")
			return
		}
		if err := importState(importFrom, stateDir); err != nil {
			fmt.Fprintln(os.Stderr, "yoakeland: import failed:", err)
			os.Exit(1)
		}
		return
	}
	from := filepath.Clean(sockpath.DefaultStateDir())
	if !offer || from == "." || from == stateDir || !needsImport(stateDir) || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	if _, err := os.Stat(from); err != nil {
		return
	}
	fmt.Printf("This is a new server, and %s has data from another yoakeland.\nCopy its projects, chats and settings into %s? The two stay separate afterwards. [y/N] ", from, stateDir)
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
		return
	}
	if err := importState(from, stateDir); err != nil {
		fmt.Fprintln(os.Stderr, "yoakeland: copy failed:", err)
		os.Exit(1)
	}
	fmt.Println("Copied.")
}
