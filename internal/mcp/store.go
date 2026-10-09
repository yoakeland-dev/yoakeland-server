package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Approval records what the user decided about one project server. It is for
// that exact config: any change to what it would run (ServerConfig.Hash) is
// asked about again.
type Approval struct {
	Hash     string `json:"hash"`
	Approved bool   `json:"approved"`
}

// Store keeps the servers the user configured for every project, and which of
// a project's own servers they approved, in <state-dir>/mcp.json (0600: a
// server's environment and headers hold secrets).
type Store struct {
	mu        sync.Mutex
	path      string
	servers   map[string]ServerConfig
	approvals map[string]map[string]Approval // project root -> server -> decision
}

type storeFile struct {
	Version   int                            `json:"version"`
	Servers   map[string]ServerConfig        `json:"servers"`
	Approvals map[string]map[string]Approval `json:"approvals,omitempty"`
}

// NewStore opens (or starts) the store in dir. A file that doesn't parse is an
// error, not a reset: it holds the user's servers.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "mcp.json"), servers: map[string]ServerConfig{}, approvals: map[string]map[string]Approval{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s doesn't parse (fix or move it): %w", s.path, err)
	}
	for name, cfg := range f.Servers {
		if ValidName(name) == nil && cfg.Validate() == nil {
			s.servers[name] = cfg
		}
	}
	if f.Approvals != nil {
		s.approvals = f.Approvals
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(storeFile{Version: 1, Servers: s.servers, Approvals: s.approvals}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

// writeFileAtomic replaces path through a temp file in the same directory, so
// a crash leaves the old file or the new one, never half of one.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Servers is a copy of the user's servers.
func (s *Store) Servers() map[string]ServerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ServerConfig, len(s.servers))
	for k, v := range s.servers {
		out[k] = v
	}
	return out
}

// Save adds or replaces a server. A rename passes the old name to drop.
func (s *Store) Save(name, replacing string, cfg ServerConfig) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.servers[name]; taken && replacing != name {
		return fmt.Errorf("there is already a server named %s", name)
	}
	prev, had := s.servers[name]
	if replacing != "" && replacing != name {
		delete(s.servers, replacing)
	}
	s.servers[name] = cfg
	if err := s.saveLocked(); err != nil {
		if had {
			s.servers[name] = prev
		} else {
			delete(s.servers, name)
		}
		return err
	}
	return nil
}

// Remove deletes a server; removing one that isn't there is not an error.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.servers[name]; !ok {
		return nil
	}
	prev := s.servers[name]
	delete(s.servers, name)
	if err := s.saveLocked(); err != nil {
		s.servers[name] = prev
		return err
	}
	return nil
}

// Approval is the user's decision on a project's server, if they made one.
func (s *Store) Approval(root, name string) (Approval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.approvals[root][name]
	return a, ok
}

// SetApproval records the decision for the config with this hash.
func (s *Store) SetApproval(root, name, hash string, approved bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approvals[root] == nil {
		s.approvals[root] = map[string]Approval{}
	}
	prev, had := s.approvals[root][name]
	s.approvals[root][name] = Approval{Hash: hash, Approved: approved}
	if err := s.saveLocked(); err != nil {
		if had {
			s.approvals[root][name] = prev
		} else {
			delete(s.approvals[root], name)
		}
		return err
	}
	return nil
}

// ForgetProject drops a project's approvals.
func (s *Store) ForgetProject(root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.approvals[root]; ok {
		delete(s.approvals, root)
		_ = s.saveLocked()
	}
}

// ProjectFile is the project's own server list, in the format Claude Code's
// .mcp.json uses.
const ProjectFile = ".mcp.json"

const maxProjectFile = 256 << 10

// LoadProjectFile reads root's .mcp.json. It goes through an os.Root so a
// symlink in a cloned repo can't point it at a file elsewhere. A missing file
// is no servers; a broken one is reported with whatever parsed.
func LoadProjectFile(root string) (servers map[string]ServerConfig, problems []string) {
	servers = map[string]ServerConfig{}
	r, err := os.OpenRoot(root)
	if err != nil {
		return servers, nil
	}
	defer r.Close()
	f, err := r.Open(ProjectFile)
	if errors.Is(err, os.ErrNotExist) {
		return servers, nil
	}
	if err != nil {
		return servers, []string{ProjectFile + ": " + err.Error()}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxProjectFile+1))
	if err != nil {
		return servers, []string{ProjectFile + ": " + err.Error()}
	}
	if len(data) > maxProjectFile {
		return servers, []string{ProjectFile + " is too large"}
	}
	var file struct {
		MCPServers map[string]ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return servers, []string{ProjectFile + " doesn't parse: " + err.Error()}
	}
	names := make([]string, 0, len(file.MCPServers))
	for n := range file.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := file.MCPServers[name]
		if err := ValidName(name); err != nil {
			problems = append(problems, fmt.Sprintf("%s: server %q: %v", ProjectFile, name, err))
			continue
		}
		if err := cfg.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("%s: server %q: %v", ProjectFile, name, err))
			continue
		}
		// A repo's file is not allowed to switch on the CLI-agent hand-off or
		// widen what the user agreed to: only the user's own config does that.
		cfg.Agents, cfg.Disabled = false, false
		servers[name] = cfg
	}
	return servers, problems
}
