package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/audit"
	"yoakeland/internal/mcp"
	"yoakeland/internal/protocol"
	"yoakeland/internal/skills"
	"yoakeland/internal/tool"
)

// MCP servers and skills over the wire (SPEC.md §3.1 "MCP servers" and
// "Skills"). Both are per-project views of a server-wide configuration: the
// user's servers and skill folders apply to every project, a project adds its
// own .mcp.json and skill folders. A change is answered to the asker and
// broadcast to every other peer, like permission rules.

// extensions is what every project shares: the user's MCP servers and sign-in
// tokens, and the skill catalog. A nil member means that part failed to open,
// and its requests say why.
type extensions struct {
	mcpStore *mcp.Store
	mcpAuth  *mcp.AuthStore
	mcpFlow  *mcp.Flow
	// mcpImports reads the servers other tools have; it needs no store, so it outlives a broken one.
	mcpImports *mcp.Importer
	skills     *skills.Catalog
	mcpErr     error
	skillErr   error
}

func newExtensions(stateDir string) *extensions {
	x := &extensions{}
	home, _ := os.UserHomeDir()
	x.mcpImports = mcp.NewImporter(mcp.DefaultImportPaths(home, os.Getenv))
	if x.mcpStore, x.mcpErr = mcp.NewStore(stateDir); x.mcpErr == nil {
		if x.mcpAuth, x.mcpErr = mcp.NewAuthStore(stateDir); x.mcpErr == nil {
			x.mcpFlow = mcp.NewFlow(x.mcpAuth)
		}
	}
	if x.mcpErr != nil {
		logWarn("MCP is off: %v", x.mcpErr)
		x.mcpStore, x.mcpAuth, x.mcpFlow = nil, nil, nil
	}
	if x.skills, x.skillErr = skills.NewCatalog(stateDir, home); x.skillErr != nil {
		logWarn("skills are off: %v", x.skillErr)
		x.skills = nil
	}
	return x
}

// projectSkills is one project's view of the catalog: what its agent's
// system prompt lists, and what the skill tool loads.
type projectSkills struct {
	cat  *skills.Catalog
	root string
}

func (p projectSkills) Skills(kind string) []skills.Skill { return p.cat.ForAgent(p.root, kind) }

func (p projectSkills) LoadSkill(name string, byUser bool) (tool.SkillDoc, error) {
	res := p.cat.Cached(p.root)
	sk, ok := skills.Find(res.Skills, name, skills.Native)
	if !ok {
		var names []string
		for _, s := range res.Skills {
			if s.Active() && s.For(skills.Native) && (byUser || !s.DisableModelInvocation) {
				names = append(names, s.Name)
			}
		}
		sort.Strings(names)
		return tool.SkillDoc{}, fmt.Errorf("there is no skill named %q (available: %s)", name, strings.Join(names, ", "))
	}
	if !byUser && sk.DisableModelInvocation {
		return tool.SkillDoc{}, fmt.Errorf("the skill %q can only be started by the user", name)
	}
	if byUser && !sk.UserInvocable {
		return tool.SkillDoc{}, fmt.Errorf("the skill %q isn't meant to be started by hand", name)
	}
	doc, err := skills.Read(sk)
	if err != nil {
		return tool.SkillDoc{}, fmt.Errorf("could not read skill %q: %w", name, err)
	}
	return tool.SkillDoc{Name: sk.Name, Dir: sk.Dir, Body: doc.Body, Files: doc.Files, Truncated: doc.Truncated}, nil
}

// mcpNotifier turns a burst of status changes (a server connecting, then
// listing tools, then another doing the same) into one push.
type mcpNotifier struct {
	mu    sync.Mutex
	timer *time.Timer
}

func (n *mcpNotifier) poke(f func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.timer != nil {
		return
	}
	n.timer = time.AfterFunc(150*time.Millisecond, func() {
		n.mu.Lock()
		n.timer = nil
		n.mu.Unlock()
		f()
	})
}

// --- MCP ---

// mcpRequest is the payload of every MCP request; each type reads the fields it names.
type mcpRequest struct {
	ProjectPath string            `json:"projectPath"`
	Name        string            `json:"name"`
	Replacing   string            `json:"replacing"`
	Config      *mcp.ServerConfig `json:"config"`
	Enabled     *bool             `json:"enabled"`
	Tool        string            `json:"tool"`
	Connect     bool              `json:"connect"`
	CallbackURL string            `json:"callbackUrl"`
	RequestID   string            `json:"requestId"`
	Code        string            `json:"code"`
	State       string            `json:"state"`
	Iss         string            `json:"iss"`
	Items       []mcp.ImportItem  `json:"items"`
}

type mcpServersPayload struct {
	ProjectPath string             `json:"projectPath"`
	Servers     []mcp.ServerStatus `json:"servers"`
	Problems    []string           `json:"problems,omitempty"`
	// Error is why the request just made failed (or why MCP is off).
	Error string `json:"error,omitempty"`
	// Redacted is what stands in for a secret value in a server's config.
	Redacted string `json:"redacted"`
}

func mcpPayload(pc *ProjectContext) mcpServersPayload {
	servers, problems := pc.MCP.Status()
	if servers == nil {
		servers = []mcp.ServerStatus{}
	}
	return mcpServersPayload{ProjectPath: pc.Root, Servers: servers, Problems: problems, Redacted: mcp.Redacted}
}

func handleMCPRequest(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, peerID string, env *protocol.Envelope) {
	var req mcpRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: "malformed " + string(env.Type) + " payload"})
		return
	}
	path := req.ProjectPath
	if path == "" {
		path = deps.defaultProjectRoot
	}
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventMCPServers, env.SessionID, env.TurnID, "", mcpServersPayload{ProjectPath: path, Servers: []mcp.ServerStatus{}, Error: msg, Redacted: mcp.Redacted})
	}
	if path == "" {
		fail("no project")
		return
	}
	pc, err := deps.projects.Get(path)
	if err != nil {
		fail(err.Error())
		return
	}
	if pc.MCP == nil {
		fail("MCP is unavailable: " + deps.projects.ext.mcpErr.Error())
		return
	}
	record := func(action, name, outcome string, detail map[string]any) {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["action"] = action
		deps.record(ipc, peerID, audit.Event{Kind: audit.MCPServer, Project: pc.Root, Target: name, Outcome: outcome, Detail: detail})
	}
	var runErr error
	mutated := true
	switch env.Type {
	case protocol.EventListMCPServers:
		mutated = false
		if req.Connect {
			pc.MCP.ConnectAll()
		}
	case protocol.EventSaveMCPServer:
		if req.Config == nil {
			runErr = fmt.Errorf("no server config")
			break
		}
		name := strings.TrimSpace(req.Name)
		if runErr = pc.MCP.Save(name, req.Replacing, *req.Config); runErr == nil {
			// Which server and what kind; never the command line or its values.
			record("save", name, "ok", map[string]any{"type": req.Config.EffectiveType(), "renamedFrom": req.Replacing})
		}
	case protocol.EventRemoveMCPServer:
		if runErr = pc.MCP.Remove(req.Name); runErr == nil {
			record("remove", req.Name, "ok", nil)
		}
	case protocol.EventImportMCPServers:
		var plan []mcp.PlannedImport
		if plan, runErr = deps.projects.ext.mcpImports.Plan(pc.Root, deps.projects.ext.mcpStore.Servers(), req.Items); runErr != nil {
			break
		}
		// All or nothing: one that fails to save takes the ones just added back out.
		for i, p := range plan {
			if runErr = pc.MCP.Save(p.Name, "", p.Config); runErr != nil {
				for _, done := range plan[:i] {
					_ = pc.MCP.Remove(done.Name)
				}
				break
			}
		}
		if runErr == nil {
			for _, p := range plan {
				record("import", p.Name, "ok", map[string]any{"type": p.Config.EffectiveType(), "importedFrom": p.Source})
			}
		}
	case protocol.EventSetMCPServerEnabled:
		if req.Enabled == nil {
			runErr = fmt.Errorf("enabled is required")
			break
		}
		var action string
		if action, runErr = setMCPServerEnabled(pc, req.Name, *req.Enabled); runErr == nil {
			record(action, req.Name, "ok", map[string]any{"enabled": *req.Enabled})
		}
	case protocol.EventSetMCPToolEnabled:
		if req.Enabled == nil || req.Tool == "" {
			runErr = fmt.Errorf("tool and enabled are required")
			break
		}
		if runErr = pc.MCP.SetToolEnabled(req.Name, req.Tool, *req.Enabled); runErr == nil {
			record("tool", req.Name, "ok", map[string]any{"tool": req.Tool, "enabled": *req.Enabled})
		}
	case protocol.EventReconnectMCPServer:
		runErr = pc.MCP.Reconnect(req.Name)
	case protocol.EventMCPSignOut:
		if runErr = pc.MCP.SignOut(req.Name); runErr == nil {
			record("sign_out", req.Name, "ok", nil)
		}
	case protocol.EventMCPOAuthStart:
		mutated = false
		id, url, err := pc.MCP.BeginAuth(ctx, req.Name, req.CallbackURL)
		if err != nil {
			fail(err.Error())
			return
		}
		_ = ipc.SendEvent(protocol.EventMCPOAuthURL, env.SessionID, env.TurnID, "", map[string]string{"requestId": id, "name": req.Name, "url": url, "projectPath": pc.Root})
		return
	case protocol.EventMCPOAuthComplete:
		var name string
		if name, runErr = pc.MCP.CompleteAuth(ctx, req.RequestID, req.Code, req.State, req.Iss); runErr == nil {
			record("sign_in", name, "ok", nil)
		}
	}
	if runErr != nil {
		if mutated {
			record(string(env.Type), req.Name, "error", map[string]any{"error": runErr.Error()})
		}
		out := mcpPayload(pc)
		out.Error = runErr.Error()
		_ = ipc.SendEvent(protocol.EventMCPServers, env.SessionID, env.TurnID, "", out)
		return
	}
	out := mcpPayload(pc)
	_ = ipc.SendEvent(protocol.EventMCPServers, env.SessionID, env.TurnID, "", out)
	if mutated && deps.peers != nil && peerID != "" {
		deps.peers.BroadcastEvent(peerID, protocol.EventMCPServers, "", out)
	}
}

type mcpImportPayload struct {
	RequestID   string                `json:"requestId"`
	ProjectPath string                `json:"projectPath"`
	Candidates  []mcp.ImportCandidate `json:"candidates"`
	Problems    []string              `json:"problems,omitempty"`
	Error       string                `json:"error,omitempty"`
	Redacted    string                `json:"redacted"`
}

// handleMCPScan answers scan_mcp_imports to the asker alone. It reads files and starts nothing.
func handleMCPScan(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope) {
	var req mcpRequest
	_ = json.Unmarshal(env.Data, &req)
	path := req.ProjectPath
	if path == "" {
		path = deps.defaultProjectRoot
	}
	out := mcpImportPayload{RequestID: req.RequestID, ProjectPath: path, Candidates: []mcp.ImportCandidate{}, Redacted: mcp.Redacted}
	if ext := deps.projects.ext; ext.mcpStore == nil {
		out.Error = "MCP is unavailable: " + ext.mcpErr.Error()
	} else {
		out.Candidates, out.Problems = ext.mcpImports.Scan(path, ext.mcpStore.Servers())
		if out.Candidates == nil {
			out.Candidates = []mcp.ImportCandidate{}
		}
	}
	_ = ipc.SendEvent(protocol.EventMCPImportCandidates, env.SessionID, env.TurnID, "", out)
}

// setMCPServerEnabled switches the user's own server on or off, or records the
// user's decision on a project's: approving runs what its .mcp.json names. It
// returns what was done, for the audit log: "enable", "disable", "approve" or "deny".
func setMCPServerEnabled(pc *ProjectContext, name string, enabled bool) (string, error) {
	servers, _ := pc.MCP.Status()
	for _, s := range servers {
		if s.Name != name {
			continue
		}
		if s.Scope == mcp.ScopeProject {
			if enabled {
				return "approve", pc.MCP.Approve(name, true)
			}
			return "deny", pc.MCP.Approve(name, false)
		}
		if enabled {
			return "enable", pc.MCP.SetEnabled(name, true)
		}
		return "disable", pc.MCP.SetEnabled(name, false)
	}
	return "", fmt.Errorf("no server named %s", name)
}

// --- skills ---

type skillsRequest struct {
	ProjectPath string `json:"projectPath"`
	Dir         string `json:"dir"`
	Enabled     *bool  `json:"enabled"`
}

type skillRoot struct {
	Path   string       `json:"path"`
	Source string       `json:"source"`
	Scope  skills.Scope `json:"scope"`
	Exists bool         `json:"exists"`
}

type skillsPayload struct {
	ProjectPath string         `json:"projectPath"`
	Skills      []skills.Skill `json:"skills"`
	Roots       []skillRoot    `json:"roots"`
	Problems    []string       `json:"problems,omitempty"`
	Error       string         `json:"error,omitempty"`
}

type skillDetailPayload struct {
	ProjectPath string   `json:"projectPath"`
	Dir         string   `json:"dir"`
	Name        string   `json:"name,omitempty"`
	Body        string   `json:"body,omitempty"`
	Files       []string `json:"files,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	Error       string   `json:"error,omitempty"`
}

func skillsFor(cat *skills.Catalog, root string) skillsPayload {
	res := cat.Scan(root)
	out := skillsPayload{ProjectPath: root, Skills: res.Skills, Problems: res.Problems, Roots: []skillRoot{}}
	if out.Skills == nil {
		out.Skills = []skills.Skill{}
	}
	for _, r := range res.Roots {
		_, err := os.Stat(r.Path)
		out.Roots = append(out.Roots, skillRoot{Path: r.Path, Source: r.Source, Scope: r.Scope, Exists: err == nil})
	}
	return out
}

func handleSkillsRequest(ipc *protocol.IPCStream, deps connDeps, peerID string, env *protocol.Envelope) {
	var req skillsRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: "malformed " + string(env.Type) + " payload"})
		return
	}
	path := req.ProjectPath
	if path == "" {
		path = deps.defaultProjectRoot
	}
	fail := func(msg string) {
		if env.Type == protocol.EventGetSkill {
			_ = ipc.SendEvent(protocol.EventSkillDetail, env.SessionID, env.TurnID, "", skillDetailPayload{ProjectPath: path, Dir: req.Dir, Error: msg})
			return
		}
		_ = ipc.SendEvent(protocol.EventSkills, env.SessionID, env.TurnID, "", skillsPayload{ProjectPath: path, Skills: []skills.Skill{}, Roots: []skillRoot{}, Error: msg})
	}
	cat := deps.projects.ext.skills
	if cat == nil {
		fail("skills are unavailable: " + deps.projects.ext.skillErr.Error())
		return
	}
	root := ""
	if path != "" {
		pc, err := deps.projects.Get(path)
		if err != nil {
			fail(err.Error())
			return
		}
		root = pc.Root
		path = pc.Root
	}
	switch env.Type {
	case protocol.EventGetSkill:
		for _, sk := range cat.Scan(root).Skills {
			if sk.Dir != req.Dir {
				continue
			}
			doc, err := skills.Read(sk)
			if err != nil {
				fail(err.Error())
				return
			}
			_ = ipc.SendEvent(protocol.EventSkillDetail, env.SessionID, env.TurnID, "", skillDetailPayload{ProjectPath: path, Dir: sk.Dir, Name: sk.Name, Body: doc.Body, Files: doc.Files, Truncated: doc.Truncated})
			return
		}
		fail("no skill at " + req.Dir)
		return
	case protocol.EventSetSkillEnabled:
		if req.Enabled == nil {
			fail("enabled is required")
			return
		}
		if err := cat.SetEnabled(root, filepath.Clean(req.Dir), *req.Enabled); err != nil {
			fail(err.Error())
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.SkillChange, Project: root, Target: filepath.Base(req.Dir), Outcome: "ok", Detail: map[string]any{"enabled": *req.Enabled}})
		out := skillsFor(cat, root)
		_ = ipc.SendEvent(protocol.EventSkills, env.SessionID, env.TurnID, "", out)
		if deps.peers != nil && peerID != "" {
			deps.peers.BroadcastEvent(peerID, protocol.EventSkills, "", out)
		}
		return
	}
	_ = ipc.SendEvent(protocol.EventSkills, env.SessionID, env.TurnID, "", skillsFor(cat, root))
}
