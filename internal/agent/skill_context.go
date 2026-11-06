package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"yoakeland/internal/skills"
	"yoakeland/internal/tool"
)

// How the skills a conversation has brought in reach a model API (SPEC.md §4h).
// The model loads one itself with the skill tool, and its result sits in the
// history like any tool result. A user's $name instead travels with the message
// that named it, in the request view only: nothing is written to history (the
// $name chip the transcript shows is the record), so no tool_use/tool_result
// pair that no provider produced ever reaches a provider.

// maxPreloaded bounds how many skills one message can load up front, and how
// many a compaction carries over.
const maxPreloaded = 5

// skillReader returns a reader of the skills a user's $name loads, which reads
// each at most once however many round-trips of the turn ask.
func (a *Agent) skillReader() func(name string) string {
	a.mu.Lock()
	loader, _ := a.skillSrc.(tool.SkillLoader)
	a.mu.Unlock()
	docs := map[string]string{}
	return func(name string) string {
		if body, ok := docs[name]; ok {
			return body
		}
		body := fmt.Sprintf("[Skill %s could not be loaded]", name)
		if loader != nil {
			if doc, err := loader.LoadSkill(name, true); err != nil {
				body = fmt.Sprintf("[Skill %s could not be loaded: %s]", name, err)
			} else {
				body = tool.FormatSkill(doc)
			}
		}
		docs[name] = body
		return body
	}
}

// withNamedSkills puts each skill's instructions into the request view once,
// where the conversation first brings it in: on the message that names it with
// $name, or on the summary standing in for a compacted conversation that did.
// A skill the model loaded itself, or one already placed, is not repeated (the
// same instructions twice only cost context). The tool exchanges pre-2.36
// sessions hold for $name (assistant calls whose ID yoakeland minted as
// skill_<turnId>_<letter>) are dropped from the view; the message that named
// the skill carries its text again.
func withNamedSkills(view []Message, have []skills.Skill, carried []string, read func(string) string) []Message {
	out := make([]Message, 0, len(view))
	placed := map[string]bool{}
	for _, m := range view {
		if isPreloadMessage(m) {
			continue
		}
		var want []string
		switch {
		case len(out) == 0 && strings.HasPrefix(m.Content, summaryOpening):
			want = carried
		case m.Role == "assistant":
			for _, tc := range m.ToolCalls {
				if name := skillCallName(tc); name != "" {
					placed[name] = true
				}
			}
		case m.Role == "user" && strings.Contains(m.Content, "$"):
			want = mentionedSkills(m.Content, have)
			if len(want) > maxPreloaded {
				want = want[:maxPreloaded]
			}
		}
		var bodies []string
		for _, name := range want {
			if !placed[name] {
				placed[name] = true
				bodies = append(bodies, read(name))
			}
		}
		if len(bodies) > 0 {
			m.Content += "\n\n" + strings.Join(bodies, "\n\n")
		}
		out = append(out, m)
	}
	return out
}

// carriedSkills are the skills a summary compaction folded away (most recent
// last, at most maxPreloaded), whose instructions the summary must not lose.
func carriedSkills(history []Message, have []skills.Skill) []string {
	cut := lastSummary(history)
	if cut < 0 {
		return nil
	}
	names := skillsBroughtIn(history[:cut], have)
	return names[max(0, len(names)-maxPreloaded):]
}

// skillsBroughtIn lists, oldest first without repeats, the skills msgs brought
// in: a user's $name, or the model's own call to the skill tool.
func skillsBroughtIn(msgs []Message, have []skills.Skill) []string {
	exists := map[string]bool{}
	for _, sk := range have {
		exists[sk.Name] = true
	}
	var out []string
	add := func(name string) {
		if exists[name] && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			for _, name := range mentionedSkills(m.Content, have) {
				add(name)
			}
		case "assistant":
			for _, tc := range m.ToolCalls {
				add(skillCallName(tc))
			}
		}
	}
	return out
}

// skillCallName is the skill a call to the skill tool asked for, or "".
func skillCallName(tc ToolCall) string {
	if tc.Name != tool.SkillToolName {
		return ""
	}
	var args struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(tc.Args, &args)
	return strings.TrimPrefix(strings.TrimSpace(args.Name), "$")
}

// isPreloadMessage: an assistant turn whose only content is a fabricated skill
// load (pre-2.36 sessions kept these in history), or its tool result.
func isPreloadMessage(m Message) bool {
	if m.Role == "tool" {
		return strings.HasPrefix(m.ToolCallID, "skill_")
	}
	return m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 1 && isPreloadCall(m.ToolCalls[0])
}

// isPreloadCall: a skill call a pre-2.36 turn made up on the user's behalf (the
// "skill_" + turnID + "_" prefix is ours by construction — a provider id never
// looks like it).
func isPreloadCall(tc ToolCall) bool {
	return tc.Name == tool.SkillToolName && strings.HasPrefix(tc.ID, "skill_")
}

// skillResultIDs are the IDs of the calls to the skill tool: a clearing
// compaction leaves their results alone, since a skill's instructions are not
// something to run again.
func skillResultIDs(history []Message) map[string]bool {
	ids := map[string]bool{}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.Name == tool.SkillToolName {
				ids[tc.ID] = true
			}
		}
	}
	return ids
}

// withSkillNames limits the skill tool's name to the skills the model may load,
// so a made-up name never reaches it.
func (a *Agent) withSkillNames(defs []tool.ToolDefinition) []tool.ToolDefinition {
	a.mu.Lock()
	src := a.skillSrc
	a.mu.Unlock()
	if src == nil {
		return defs
	}
	var names []string
	for _, sk := range src.Skills(skills.Native) {
		if !sk.DisableModelInvocation {
			names = append(names, sk.Name)
		}
	}
	for i, def := range defs {
		if def.Name != tool.SkillToolName || len(names) == 0 {
			continue
		}
		props, _ := def.Parameters["properties"].(map[string]any)
		name, _ := props["name"].(map[string]any)
		if name == nil {
			break
		}
		name = maps.Clone(name)
		name["enum"] = names
		props = maps.Clone(props)
		props["name"] = name
		params := maps.Clone(def.Parameters)
		params["properties"] = props
		defs = slices.Clone(defs)
		defs[i].Parameters = params
		break
	}
	return defs
}
