package provider

import (
	"regexp"
	"strings"

	"yoakeland/internal/agent"
)

// Skills are found by each CLI agent on its own, in its own folders. What
// yoakeland adds is the way to ask for one: the composer writes $name, and a
// CLI agent is told in the form it understands.

// SkillKind names the agent the way the skill folders do (skills.Roots).
func (p *CLISubprocessProvider) SkillKind() string { return string(p.Kind) }

// SkillPrompt rewrites a message that names skills with $name for this agent.
//
//   - Claude Code runs a skill from a message that starts with /name; what follows
//     becomes its arguments. A message names one skill that way, and any more are
//     left in the text as /name for the model to pick up with its Skill tool.
//   - Codex reads $name in the text itself, so the message goes through as typed.
//   - Pi and Oh My Pi take /skill:name at the start.
//   - The rest find skills on their own and have no hand-invoked form that has been
//     checked; they are told in words which skill the user means.
func (p *CLISubprocessProvider) SkillPrompt(prompt string, names []string) string {
	switch p.Kind {
	case CLIKindCodex:
		return prompt
	case CLIKindClaude:
		return leadWith(prompt, names, "/")
	case CLIKindPi, CLIKindOhMyPi:
		return leadWith(prompt, names, "/skill:")
	}
	body := replaceMentions(prompt, names, func(n string) string { return n })
	word := "skill"
	if len(names) > 1 {
		word = "skills"
	}
	return "Use the " + strings.Join(names, ", ") + " " + word + " for this.\n\n" + body
}

// leadWith moves the last named skill to the front as prefix+name and leaves
// the others in the text, written with the same prefix.
func leadWith(prompt string, names []string, prefix string) string {
	last := names[len(names)-1]
	rest := replaceMentions(prompt, names, func(n string) string {
		if n == last {
			return "\x00"
		}
		return prefix + n
	})
	rest = strings.Join(strings.Fields(strings.ReplaceAll(rest, "\x00", "")), " ")
	if strings.TrimSpace(rest) == "" {
		return prefix + last
	}
	return prefix + last + " " + rest
}

var mentionRe = regexp.MustCompile(`(^|\s)\$([A-Za-z][A-Za-z0-9._:-]{0,63})($|[\s.,;:!?)])`)

// replaceMentions rewrites $name for each name in names, leaving every other $
// (money, variables) alone.
func replaceMentions(text string, names []string, to func(string) string) string {
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	// Two adjacent mentions share a separator, which one pass would consume
	// once; running until nothing changes handles that.
	for {
		next := mentionRe.ReplaceAllStringFunc(text, func(m string) string {
			sub := mentionRe.FindStringSubmatch(m)
			if !known[sub[2]] {
				return m
			}
			return sub[1] + to(sub[2]) + sub[3]
		})
		if next == text {
			return text
		}
		text = next
	}
}

var _ agent.NativeSkillAgent = (*CLISubprocessProvider)(nil)
