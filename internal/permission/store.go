package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FilePermissionsModel is the permissions.json shape. Version 2 stores an
// array of Rules; version 1 (exact-match entries) is converted on load.
type FilePermissionsModel struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

type legacyRule struct {
	Allowed   bool      `json:"allowed"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PersistentStore is one rules file: a project's permissions.json, or the
// global permissions.global.json.
type PersistentStore struct {
	mu       sync.Mutex
	filePath string
	scope    Scope
}

func NewPersistentStore(dataDir string) *PersistentStore {
	_ = os.MkdirAll(dataDir, 0o700)
	return &PersistentStore{filePath: filepath.Join(dataDir, "permissions.json"), scope: ScopeProject}
}

func NewGlobalStore(stateDir string) *PersistentStore {
	_ = os.MkdirAll(stateDir, 0o700)
	return &PersistentStore{filePath: filepath.Join(stateDir, "permissions.global.json"), scope: ScopeGlobal}
}

// Load reads the file. A missing file is no rules. A file that cannot be
// parsed, is a newer version, or holds a rule that fails validation is an
// error: the caller must fail closed, not carry on with fewer rules (a
// dropped deny would silently become an ask). A v1 file is migrated: the
// original is copied to <file>.v1.bak, then the converted v2 is written.
// notes lists v1 entries that had no safe v2 equivalent and were dropped.
func (s *PersistentStore) Load(root string) (rules []Rule, notes []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var head struct {
		Version int             `json:"version"`
		Rules   json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, nil, fmt.Errorf("malformed %s: %w", filepath.Base(s.filePath), err)
	}
	switch head.Version {
	case 1:
		var old map[string]legacyRule
		if err := json.Unmarshal(head.Rules, &old); err != nil {
			return nil, nil, fmt.Errorf("malformed %s: %w", filepath.Base(s.filePath), err)
		}
		rules, notes = migrateV1(old, root)
		bak := s.filePath + ".v1.bak"
		if _, statErr := os.Stat(bak); statErr == nil {
			// A backup of an earlier v1 already exists; keep that one.
		} else if werr := os.WriteFile(bak, data, 0o600); werr != nil {
			return rules, append(notes, "could not back up the old file ("+werr.Error()+"); migrated rules are in memory only"), nil
		}
		if werr := s.save(rules); werr != nil {
			notes = append(notes, "could not write the migrated file: "+werr.Error())
		}
		return rules, notes, nil
	case 2:
		if err := json.Unmarshal(head.Rules, &rules); err != nil {
			return nil, nil, fmt.Errorf("malformed %s: %w", filepath.Base(s.filePath), err)
		}
		for i := range rules {
			if rules[i].ID == "" {
				return nil, nil, fmt.Errorf("%s: rule %d has no id", filepath.Base(s.filePath), i+1)
			}
			if err := rules[i].Validate(s.scope); err != nil {
				return nil, nil, fmt.Errorf("%s: rule %s: %w", filepath.Base(s.filePath), rules[i].ID, err)
			}
		}
		return rules, nil, nil
	default:
		return nil, nil, fmt.Errorf("%s has version %d, which this yoakeland does not understand", filepath.Base(s.filePath), head.Version)
	}
}

func (s *PersistentStore) Save(rules []Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(rules)
}

func (s *PersistentStore) save(rules []Rule) error {
	if rules == nil {
		rules = []Rule{}
	}
	out := make([]Rule, len(rules))
	copy(out, rules)
	for i := range out {
		out[i].Scope = ""
	}
	b, err := json.MarshalIndent(FilePermissionsModel{Version: 2, Rules: out}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.filePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

// migrateV1 turns exact "tool:target" entries into v2 rules that mean
// exactly the same thing: a bash command becomes its exact argv (no
// wildcard), a file becomes that one path. Anything that cannot be
// expressed safely is dropped and listed in notes.
func migrateV1(old map[string]legacyRule, root string) ([]Rule, []string) {
	keys := make([]string, 0, len(old))
	for k := range old {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rules []Rule
	var notes []string
	for _, k := range keys {
		e := old[k]
		toolName, target, ok := strings.Cut(k, ":")
		if !ok {
			notes = append(notes, "dropped malformed entry "+k)
			continue
		}
		r := Rule{ID: newRuleID(), Tool: toolName, Decision: DecisionDeny, Source: SourceMigrated, CreatedAt: e.UpdatedAt}
		if e.Allowed {
			r.Decision = DecisionAllow
		}
		switch toolName {
		case ToolBash:
			sc, err := parseArgv(target)
			if err != nil || len(sc.Argv) == 0 || (e.Allowed && (sc.HasChain || contains(sc.Argv, "*"))) {
				notes = append(notes, "dropped bash entry that cannot be an exact argv rule: "+target)
				continue
			}
			r.Pattern = sc.Argv
		case ToolFileWrite, ToolFileEdit:
			rel := relSlash(root, target)
			if rel == "" {
				notes = append(notes, "dropped "+toolName+" entry outside the project: "+target)
				continue
			}
			r.Pattern = []string{escapeGlob(rel)}
		case ToolWebFetch, ToolBrowser:
			r.Pattern = []string{target}
		default:
			notes = append(notes, "dropped entry for "+toolName+": rules do not cover it")
			continue
		}
		if err := r.Validate(ScopeProject); err != nil {
			notes = append(notes, fmt.Sprintf("dropped %s entry %q: %v", toolName, target, err))
			continue
		}
		rules = append(rules, r)
	}
	return rules, notes
}

// escapeGlob makes a literal path safe to use as a path.Match pattern.
func escapeGlob(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r == '*' || r == '?' || r == '[' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
