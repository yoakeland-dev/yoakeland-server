package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/skills"
	"yoakeland/internal/tool"
)

// What a project adds to an agent beyond its fixed tools: MCP servers (a
// tool.Source whose tools come and go) and skills (instructions the model loads
// when a task calls for them).

// SkillSource tells an agent which skills it may use in its project.
type SkillSource interface {
	// Skills lists the active skills the agent kind (skills.Native for
	// yoakeland's own loop, else a provider kind) can load, newest scan.
	Skills(agentKind string) []skills.Skill
}

// SetToolSource makes src's tools available to every session, current and
// future.
func (r *Registry) SetToolSource(src tool.Source) {
	r.mu.Lock()
	r.source = src
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.SetToolSource(src)
	}
}

// SetToolSource gives this session a source of tools.
func (a *Agent) SetToolSource(src tool.Source) {
	a.mu.Lock()
	a.source = src
	a.mu.Unlock()
}

// SetSkills gives this session a source of skills.
func (a *Agent) SetSkills(src SkillSource) {
	a.mu.Lock()
	a.skillSrc = src
	a.mu.Unlock()
}

// SetSkills lets every session, current and future, use src's skills.
// SetNativeServers adds MCP servers of yoakeland's own to what a CLI agent is handed each turn (the
// browser tool, which such an agent can't be given as a plain tool). Call before sessions open.
func (r *Registry) SetNativeServers(f func(sessionID string) []tool.AgentServer) {
	r.mu.Lock()
	r.nativeServers = f
	r.mu.Unlock()
}

func (r *Registry) SetSkills(src SkillSource) {
	r.mu.Lock()
	r.skillSrc = src
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.SetSkills(src)
	}
}

func (a *Agent) toolSource() tool.Source {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.source
}

// sourceTools is what the source offers now.
func (a *Agent) sourceTools() []tool.Tool {
	if src := a.toolSource(); src != nil {
		return src.Tools()
	}
	return nil
}

// prepareSource starts whatever the source hasn't and waits a bounded time, so
// a turn's first request already has the tools of servers that start quickly.
func (a *Agent) prepareSource(ctx context.Context) {
	src := a.toolSource()
	if src == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	src.Prepare(pctx)
}

// lookupTool finds a tool by name: the fixed ones, then the source's.
func (a *Agent) lookupTool(name string) (tool.Tool, bool) {
	if t, ok := a.tools[name]; ok {
		return t, true
	}
	for _, t := range a.sourceTools() {
		if t.Name() == name {
			return t, true
		}
	}
	return nil, false
}

// offeredByMode: whether a source tool is given to the model in mode. Read-only
// and plan modes get only the tools whose server says they change nothing; that
// claim only narrows what is offered, never what runs without asking.
func offeredByMode(t tool.Tool, mode Mode) bool {
	if mode != ModeReadOnly && mode != ModePlan {
		return true
	}
	ro, ok := t.(tool.ReadOnlyTool)
	return ok && ro.ReadOnly()
}

// sourceDefs are the definitions of the source's tools offered in mode.
func (a *Agent) sourceDefs(mode Mode) []tool.ToolDefinition {
	var defs []tool.ToolDefinition
	for _, t := range a.sourceTools() {
		if offeredByMode(t, mode) {
			defs = append(defs, t.Definition())
		}
	}
	return defs
}

// skillKind is the name a skill's folders know this agent by.
func skillKind(p ModelProvider) string {
	if sa, ok := p.(NativeSkillAgent); ok {
		return sa.SkillKind()
	}
	return skills.Native
}

// NativeSkillAgent is a CLI agent that loads skills itself, and knows how it is
// told to run one.
type NativeSkillAgent interface {
	NativeAgent
	// SkillKind names the agent in the skill folders' vocabulary ("claude").
	SkillKind() string
	// SkillPrompt rewrites a user message that names skills with $name into
	// what the agent understands, given the names it may use.
	SkillPrompt(prompt string, names []string) string
}

// skillsFor lists the skills this agent's provider may use now.
func (a *Agent) skillsFor(p ModelProvider) []skills.Skill {
	a.mu.Lock()
	src := a.skillSrc
	a.mu.Unlock()
	if src == nil {
		return nil
	}
	return src.Skills(skillKind(p))
}

// skillsAvailable: the skill tool is only offered when there is something to load.
func (a *Agent) skillsAvailable() bool {
	a.mu.Lock()
	src := a.skillSrc
	a.mu.Unlock()
	return src != nil && len(src.Skills(skills.Native)) > 0
}

// skillDirs are the folders of the skills this agent can use, which file_read
// may read even outside the project (a user's skill lives in their home).
func (a *Agent) skillDirs() []string {
	a.mu.Lock()
	src := a.skillSrc
	a.mu.Unlock()
	if src == nil {
		return nil
	}
	var dirs []string
	for _, sk := range src.Skills(skills.Native) {
		dirs = append(dirs, sk.Dir)
	}
	return dirs
}

// skillMention is $name: a skill the user asked for in their message. The
// pattern leaves "$5" and "$HOME/x" alone; what it matches is then checked
// against the skills that exist, so only a real skill's name does anything.
var skillMention = regexp.MustCompile(`(^|\s)\$([A-Za-z][A-Za-z0-9._:-]{0,63})(?:$|[\s.,;:!?)])`)

// mentionedSkills returns the distinct names in text that are skills in have, in order.
func mentionedSkills(text string, have []skills.Skill) []string {
	known := map[string]bool{}
	for _, sk := range have {
		if sk.UserInvocable {
			known[sk.Name] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range skillMention.FindAllStringSubmatch(text, -1) {
		if name := m[2]; known[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// maxPreloaded bounds how many skills one message can load up front.
const maxPreloaded = 5

// preloadSkills loads the skills the user named with $name into the turn as if
// the model had called the skill tool for each, so the call and its result show
// in the chat and stay in the history. Only for a model API: a CLI agent is
// told through SkillPrompt instead.
func (a *Agent) preloadSkills(turnID string, bound RoleBinding) {
	have := a.skillsFor(bound.Provider)
	if len(have) == 0 {
		return
	}
	a.mu.Lock()
	var text string
	// Only right after a fresh user message: a retry keeps the earlier preload
	// in history, which would otherwise be loaded a second time.
	if n := len(a.history); n > 0 && a.history[n-1].Role == "user" {
		text = a.history[n-1].Content
	}
	a.mu.Unlock()
	names := mentionedSkills(text, have)
	if len(names) > maxPreloaded {
		names = names[:maxPreloaded]
	}
	if len(names) == 0 {
		return
	}
	a.mu.Lock()
	loader, ok := a.skillSrc.(tool.SkillLoader)
	a.mu.Unlock()
	if !ok {
		return
	}
	for i, name := range names {
		args, _ := json.Marshal(map[string]string{"name": name})
		tc := ToolCall{ID: "skill_" + turnID + "_" + string(rune('a'+i)), Name: tool.SkillToolName, Args: args}
		a.mu.Lock()
		a.history = append(a.history, Message{
			ID: a.nextMessageIDLocked(), Role: "assistant", ToolCalls: []ToolCall{tc},
			Provider: bound.ProviderName, Model: bound.Model, CreatedAt: time.Now(),
		})
		sessID := a.sessionID
		a.mu.Unlock()
		_ = a.getStream().SendEvent(protocol.EventToolCallRequested, sessID, turnID, tc.ID, protocol.ToolCallData{Name: tc.Name, Args: tc.Args})
		doc, err := loader.LoadSkill(name, true)
		if err != nil {
			a.recordToolResult(turnID, tc, "", err.Error())
			continue
		}
		a.recordToolResult(turnID, tc, tool.FormatSkill(doc), "")
	}
}

// skillPromptFor rewrites a user message for a CLI agent that takes skills its
// own way, or returns it unchanged.
func (a *Agent) skillPromptFor(p ModelProvider, text string) string {
	sa, ok := p.(NativeSkillAgent)
	if !ok || !strings.Contains(text, "$") {
		return text
	}
	names := mentionedSkills(text, a.skillsFor(p))
	if len(names) == 0 {
		return text
	}
	return sa.SkillPrompt(text, names)
}

// splitSkillsSection separates the skills listing from the rest of a system
// prompt, so a context breakdown can size it by itself.
func splitSkillsSection(system string) (rest, skillsPart string) {
	const marker = "\n\n# Skills\n"
	if i := strings.Index(system, marker); i >= 0 {
		return system[:i], system[i:]
	}
	return system, ""
}

// isSourceTool: name came from the source (an MCP tool), not the fixed set.
func isMCPTool(name string) bool { return strings.HasPrefix(name, "mcp__") }

// requestSuffix is what follows the mode's system prompt on a request: the
// project's instructions, then the skills the model may load (none for a CLI
// agent, which has its own).
func (a *Agent) requestSuffix(p ModelProvider) string {
	s := projectInstructions(a.projectRoot)
	if _, native := p.(NativeAgent); !native {
		s += skills.Listing(a.skillsFor(p))
	}
	return s
}
