package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/projectstore"
)

func TestImportStateCopiesDataNotIdentity(t *testing.T) {
	base := t.TempDir()
	from, to := filepath.Join(base, "a"), filepath.Join(base, "b")
	chat := filepath.Join(from, "chats", "c1")
	rec := filepath.Join(from, "projects", filepath.Base(projectstore.DataDir(from, chat)))
	for _, d := range []string{chat, rec, filepath.Join(from, "tls")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(rec, "project.json"), `{"path":"`+chat+`/x"}`)
	write(filepath.Join(from, "tls", "ca.key"), "secret")
	write(filepath.Join(from, "providers.json"), "{}")
	write(filepath.Join(from, "server.json"), `{"identity":"fish","serverId":"abc","runtime":{"x":1},"users":{"owner":{"id":"owner","name":"Ava","tier":"full"}},"limitedDefaults":{"mcp_tools":true},"tokens":{"t":{"token":"secret"}}}`)

	if !needsImport(to) {
		t.Fatal("a missing dir needs import")
	}
	if err := importState(from, to); err != nil {
		t.Fatal(err)
	}
	if needsImport(to) {
		t.Fatal("a finished copy is a state dir")
	}
	if _, err := os.Stat(filepath.Join(to, "tls")); err == nil {
		t.Fatal("tls must not be copied")
	}
	if b, _ := os.ReadFile(filepath.Join(to, "server.json")); strings.Contains(string(b), "serverId") || strings.Contains(string(b), "secret") || !strings.Contains(string(b), "fish") || !strings.Contains(string(b), `"Ava"`) || !strings.Contains(string(b), "limitedDefaults") {
		t.Fatalf("server.json: %s", b)
	}
	moved := filepath.Join(to, "projects", filepath.Base(projectstore.DataDir(to, filepath.Join(to, "chats", "c1"))), "project.json")
	if b, err := os.ReadFile(moved); err != nil || !strings.Contains(string(b), to+"/chats/c1/x") {
		t.Fatalf("project record not re-filed or rewritten: %v %s", err, b)
	}
}
