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

type Normalizer struct {
	ProjectRoot string
}

func New(projectRoot string) (*Normalizer, error) {
	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		// If root itself does not exist yet, fallback to clean absolute path
		realRoot = filepath.Clean(absRoot)
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

	return cleanEval, nil
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
