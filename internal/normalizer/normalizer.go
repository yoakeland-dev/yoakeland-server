package normalizer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

var (
	ErrEscapesRoot     = errors.New("path escapes project root")
	ErrDisallowedChain = errors.New("command chaining or piping is prohibited without explicit permission")
)

// Hidden lists directories no project may reach even when one contains
// them — yoakeland's own state dir (tokens, the TLS CA key, provider keys,
// every project's sessions), which a project opened at $HOME would
// otherwise include. Set once at startup, before any project opens; the
// sandbox also mounts an empty tmpfs over each (sandbox_linux.go).
var Hidden []string

// HiddenIn returns the Hidden dirs strictly inside root.
func HiddenIn(root string) []string {
	var out []string
	for _, h := range Hidden {
		if rel, err := filepath.Rel(root, h); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out = append(out, h)
		}
	}
	return out
}

type Normalizer struct {
	ProjectRoot string
}

// CanonicalRoot resolves path to an absolute, symlink-resolved form — the
// one canonicalization every piece of code that identifies a project by its
// filesystem path (the project registry's map key, what gets echoed back on
// the wire, what this Normalizer itself roots at) must agree on. Two
// differently-spelled paths to the same real directory (a symlink vs. its
// target, a trailing slash, `..` segments) must always canonicalize to the
// same string, or callers that key state off it end up treating one project
// as two.
func CanonicalRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Root doesn't exist yet (or a broken symlink) — fall back to the
		// clean absolute path rather than failing outright.
		return filepath.Clean(abs), nil
	}
	return real, nil
}

func New(projectRoot string) (*Normalizer, error) {
	realRoot, err := CanonicalRoot(projectRoot)
	if err != nil {
		return nil, err
	}
	return &Normalizer{ProjectRoot: realRoot}, nil
}

// ResolveAndValidatePath ensures the path does not escape the project boundary,
// evaluating symlinks to prevent escapes.
func (n *Normalizer) ResolveAndValidatePath(rawPath string) (string, error) {
	cleanRaw := filepath.Clean(rawPath)
	var candidate string
	if filepath.IsAbs(cleanRaw) {
		candidate = cleanRaw
	} else {
		candidate = filepath.Join(n.ProjectRoot, cleanRaw)
	}

	// Resolve symlinks progressively: evaluate existing parent if target doesn't exist yet
	evalPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			// Resolve parent dir to prevent symlink traversal trickery on newly created files
			parent := filepath.Dir(candidate)
			evalParent, pErr := filepath.EvalSymlinks(parent)
			if pErr == nil {
				evalPath = filepath.Join(evalParent, filepath.Base(candidate))
			} else {
				evalPath = candidate
			}
		} else {
			return "", fmt.Errorf("evaluating symlinks: %w", err)
		}
	}

	cleanEval := filepath.Clean(evalPath)
	rel, err := filepath.Rel(n.ProjectRoot, cleanEval)
	if err != nil {
		return "", ErrEscapesRoot
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s resolves outside %s", ErrEscapesRoot, rawPath, n.ProjectRoot)
	}
	for _, h := range HiddenIn(n.ProjectRoot) {
		if r, err := filepath.Rel(h, cleanEval); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: %s is yoakeland's own state", ErrEscapesRoot, rawPath)
		}
	}

	return cleanEval, nil
}

// InRoot runs fn on abs (a path ResolveAndValidatePath already approved)
// through an os.Root opened at root. That check is only a snapshot: a
// process in the sandbox (a dev server, a terminal) can swap a directory for
// a symlink before the actual open. os.Root resolves every component beneath
// root in the kernel, so a swapped-in link leading out fails instead of being
// followed. It also refuses absolute symlinks, which a resolved abs never
// contains.
func InRoot(root, abs string, fn func(r *os.Root, rel string) error) error {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s", ErrEscapesRoot, abs)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return fn(r, rel)
}

// ErrGitExecPath refuses a write git would later run as code on the host.
var ErrGitExecPath = errors.New("refusing to write .git/config or .git/hooks: git runs them as code outside the sandbox")

// IsGitExecPath reports whether resolved (a ResolveAndValidatePath result)
// is a .git entry itself, its config, or its hooks — anything that, once
// written, git executes the next time it runs on the host.
func (n *Normalizer) IsGitExecPath(resolved string) bool {
	rel, err := filepath.Rel(n.ProjectRoot, resolved)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.ToLower(filepath.ToSlash(rel)), "/")
	for i, p := range parts {
		if p == ".git" && (i == len(parts)-1 || parts[i+1] == "config" || parts[i+1] == "hooks") {
			return true
		}
	}
	return false
}

// ShellCommand represents parsed argv with flagged meta-operations.
type ShellCommand struct {
	Original string
	Argv     []string
	HasChain bool
}

// ParseShellArgv parses POSIX shell strings into arguments without using shell execution.
// It detects and reports command chaining operators (;, &&, ||, |, `).
func (n *Normalizer) ParseShellArgv(cmdStr string) (*ShellCommand, error) {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return nil, errors.New("empty command")
	}

	var (
		argv        []string
		curr        strings.Builder
		inSingle    bool
		inDouble    bool
		escaped     bool
		hasChaining bool
	)

	runes := []rune(cmdStr)
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if escaped {
			curr.WriteRune(r)
			escaped = false
			continue
		}

		if r == '\\' && !inSingle {
			// Inside double quotes a backslash is literal unless it escapes one of
			// the characters POSIX allows there.
			if inDouble && (i+1 >= len(runes) || !strings.ContainsRune("$`\"\\\n", runes[i+1])) {
				curr.WriteRune(r)
				continue
			}
			escaped = true
			continue
		}

		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}

		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}

		// Operator checks outside quotes
		if !inSingle && !inDouble {
			if r == ';' || r == '|' || r == '&' || r == '`' || r == '$' {
				hasChaining = true
			}

			if unicode.IsSpace(r) {
				if curr.Len() > 0 {
					argv = append(argv, curr.String())
					curr.Reset()
				}
				continue
			}
		}

		curr.WriteRune(r)
	}

	if inSingle || inDouble {
		return nil, errors.New("unclosed quotation mark in command")
	}

	if curr.Len() > 0 {
		argv = append(argv, curr.String())
	}

	return &ShellCommand{
		Original: cmdStr,
		Argv:     argv,
		HasChain: hasChaining,
	}, nil
}

// IsDestructiveBash scans parsed argv for universally catastrophic operations.
func (n *Normalizer) IsDestructiveBash(sc *ShellCommand) bool {
	if sc.HasChain {
		return true
	}
	if len(sc.Argv) == 0 {
		return false
	}

	bin := filepath.Base(sc.Argv[0])

	switch bin {
	case "rm", "srm":
		for _, arg := range sc.Argv[1:] {
			if strings.HasPrefix(arg, "-") && strings.Contains(arg, "r") {
				return true
			}
			if arg == "/" || arg == "/*" || arg == "~" {
				return true
			}
		}
	case "mkfs", "dd", "shutdown", "reboot", "poweroff":
		return true
	case "git":
		if len(sc.Argv) > 1 {
			sub := sc.Argv[1]
			if sub == "reset" || sub == "clean" || sub == "restore" {
				return true
			}
		}
	}

	return false
}
