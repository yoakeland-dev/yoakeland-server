package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/fsutil"
)

// Catalog finds skills and remembers which the user switched off. The switch is
// kept in yoakeland's own state, not by renaming the skill's files: the same
// folder is read by other agents too, and turning a skill off here must not
// take it away from them.
type Catalog struct {
	stateDir string
	home     string

	mu       sync.Mutex
	path     string
	disabled map[string]bool // by real directory
	cache    map[string]cached
}

type cached struct {
	at  time.Time
	res Result
}

// cacheTTL is how long Cached trusts a scan: a turn asks several times, and
// the folders rarely change between two requests of one turn.
const cacheTTL = 2 * time.Second

// NewCatalog opens the catalog's state in stateDir. home is where the user's own
// skill folders are ("" for none, as in a test).
func NewCatalog(stateDir, home string) (*Catalog, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	c := &Catalog{stateDir: stateDir, home: home, path: filepath.Join(stateDir, "skills.json"), disabled: map[string]bool{}, cache: map[string]cached{}}
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Disabled []string `json:"disabled"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s doesn't parse (fix or move it): %w", c.path, err)
	}
	for _, d := range f.Disabled {
		c.disabled[d] = true
	}
	return c, nil
}

func (c *Catalog) isDisabled(dir string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disabled[dir]
}

// Result is what a scan found.
type Result struct {
	Skills   []Skill
	Roots    []Root
	Problems []string
}

// Scan reads every skill folder for the project at projectRoot ("" for none).
func (c *Catalog) Scan(projectRoot string) Result {
	roots := Roots(c.stateDir, c.home, projectRoot)
	sk, problems := Scan(roots, c.isDisabled)
	return Result{Skills: sk, Roots: roots, Problems: problems}
}

// Cached is Scan, reused for a moment: for a turn's own repeated questions.
func (c *Catalog) Cached(projectRoot string) Result {
	c.mu.Lock()
	if e, ok := c.cache[projectRoot]; ok && time.Since(e.at) < cacheTTL {
		c.mu.Unlock()
		return e.res
	}
	c.mu.Unlock()
	res := c.Scan(projectRoot)
	c.mu.Lock()
	c.cache[projectRoot] = cached{time.Now(), res}
	c.mu.Unlock()
	return res
}

// SetEnabled switches a skill, named by its folder, on or off. The folder must
// be one a scan finds: a client can't use this to write arbitrary paths into
// the state file.
func (c *Catalog) SetEnabled(projectRoot, dir string, enabled bool) error {
	found := false
	for _, sk := range c.Scan(projectRoot).Skills {
		if sk.Dir == dir {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no skill at %s", dir)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = map[string]cached{}
	prev, had := c.disabled[dir]
	if enabled {
		delete(c.disabled, dir)
	} else {
		c.disabled[dir] = true
	}
	if err := c.saveLocked(); err != nil {
		if had {
			c.disabled[dir] = prev
		} else {
			delete(c.disabled, dir)
		}
		return err
	}
	return nil
}

func (c *Catalog) saveLocked() error {
	list := make([]string, 0, len(c.disabled))
	for d := range c.disabled {
		list = append(list, d)
	}
	sort.Strings(list)
	b, err := json.MarshalIndent(struct {
		Disabled []string `json:"disabled"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(c.path, b, 0600)
}

// ForAgent is the skills an agent can use in this project: switched on, not
// hidden, and in a folder that agent reads.
func (c *Catalog) ForAgent(projectRoot, agent string) []Skill {
	var out []Skill
	for _, sk := range c.Cached(projectRoot).Skills {
		if sk.Active() && sk.For(agent) {
			out = append(out, sk)
		}
	}
	return out
}

// Listing is the system-prompt section naming the skills the model may load on
// its own, or "" when there are none. Skills whose author said only the user
// may start them are left out. It stays under a size budget by shortening
// descriptions and then dropping the tail, saying so.
func Listing(skills []Skill) string {
	var usable []Skill
	for _, sk := range skills {
		if !sk.DisableModelInvocation {
			usable = append(usable, sk)
		}
	}
	if len(usable) == 0 {
		return ""
	}
	const budget = 12 << 10
	for _, descLen := range []int{maxDescription, 300, 120} {
		var sb strings.Builder
		sb.WriteString("\n\n# Skills\nSkills are folders of instructions for particular kinds of task. When a task matches a skill's description, " +
			"call the `skill` tool with its name to load the full instructions before you start, then follow them; load a skill only when it applies. " +
			"The user can also start one by writing $name in their message, in which case its instructions are already loaded for you.\n\n<available_skills>\n")
		dropped := 0
		for i, sk := range usable {
			desc := sk.Description
			if len(desc) > descLen {
				desc = strings.ToValidUTF8(desc[:descLen], "") + "…"
			}
			entry := fmt.Sprintf("  <skill>\n    <name>%s</name>\n    <description>%s</description>\n    <location>%s</location>\n  </skill>\n",
				html.EscapeString(sk.Name), html.EscapeString(desc), html.EscapeString(sk.File))
			if sb.Len()+len(entry) > budget {
				dropped = len(usable) - i
				break
			}
			sb.WriteString(entry)
		}
		sb.WriteString("</available_skills>")
		if dropped == 0 {
			return sb.String()
		}
		if descLen == 120 {
			return sb.String() + fmt.Sprintf("\n(%d more skills aren't listed; the user can name one with $name.)", dropped)
		}
	}
	return ""
}
