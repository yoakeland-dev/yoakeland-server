package agent

import (
	"os"
	"strings"

	"yoakeland/internal/normalizer"
)

// projectInstructionFiles are the project's own notes for coding agents
// (how to build, test and find things); the first one present is appended
// to the system prompt, so a session doesn't rediscover them every time.
var projectInstructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

const maxInstructionBytes = 16000

// projectInstructions returns the system-prompt section for root's
// instructions file, or "" when there is none. Whatever it reads is sent to
// the model provider, so the file goes through the same checks file_read
// does: a symlink planted in a cloned repo can't point it outside the
// project or at yoakeland's own state (normalizer.Hidden — tokens, keys),
// and it's read through an os.Root so a swap after the check can't either.
func projectInstructions(root string) string {
	if root == "" {
		return ""
	}
	norm, err := normalizer.New(root)
	if err != nil {
		return ""
	}
	for _, name := range projectInstructionFiles {
		real, err := norm.ResolveAndValidatePath(name)
		if err != nil {
			continue
		}
		var data []byte
		err = normalizer.InRoot(norm.ProjectRoot, real, func(r *os.Root, rel string) error {
			fi, err := r.Stat(rel)
			if err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return os.ErrInvalid
			}
			data, err = r.ReadFile(rel)
			return err
		})
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		if len(text) > maxInstructionBytes {
			text = text[:maxInstructionBytes] + "\n[... truncated]"
		}
		return "\n\n# Project instructions (" + name + ")\n" + text
	}
	return ""
}
