package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// An account is one login of a CLI agent. Claude Code and Codex keep theirs in a
// config folder (CLAUDE_CONFIG_DIR / CODEX_HOME), so a second account is the same
// binary started with another folder. Every other agent has one login per machine.

// configDirVar is the variable that points a CLI at its account folder.
var configDirVar = map[CLIKind]string{
	CLIKindClaude: "CLAUDE_CONFIG_DIR",
	CLIKindCodex:  "CODEX_HOME",
}

// blockedEnv are variables a custom agent may not set: they change what the
// server's own process loads, not what the agent is.
func blockedEnv(k string) bool {
	k = strings.ToUpper(k)
	return strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "DYLD_") ||
		k == "PATH" || k == "NODE_OPTIONS" || k == "BASH_ENV" || k == "ENV" || k == "IFS"
}

// procEnv is the extra environment that makes the CLI run as this account.
func procEnv(kind CLIKind, configDir string, env map[string]string) []string {
	var out []string
	if v := configDirVar[kind]; v != "" && configDir != "" {
		out = append(out, v+"="+configDir)
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

type procEnvKey struct{}

// withProcEnv carries an account's environment to startProc through the
// context, so no starter signature or call site has to change.
func withProcEnv(ctx context.Context, env []string) context.Context {
	if len(env) == 0 {
		return ctx
	}
	return context.WithValue(ctx, procEnvKey{}, env)
}

func procEnvFrom(ctx context.Context) []string {
	env, _ := ctx.Value(procEnvKey{}).([]string)
	return env
}

// envStarter runs every process start with env added.
func envStarter(base procStarter, env []string) procStarter {
	if len(env) == 0 {
		return base
	}
	return func(ctx context.Context, name string, args []string, dir string) (*nativeProc, error) {
		return base(withProcEnv(ctx, env), name, args, dir)
	}
}

// AccountKey identifies the login behind a provider: two providers with the same
// key share a session pool slot, model list and usage probe.
func (cfg ProviderConfig) AccountKey() CLIKind {
	kind, _ := CLIKindOf(cfg.Kind)
	switch {
	case kind == CLIKindACP:
		return CLIKind(string(kind) + ":" + cfg.Command + "\x00" + strings.Join(cfg.Args, "\x00"))
	case cfg.ConfigDir != "" && configDirVar[kind] != "":
		return CLIKind(string(kind) + ":" + cfg.ConfigDir)
	}
	return kind
}

// EnvKeys are the names of the custom environment, for clients: the values stay on the server.
func (cfg ProviderConfig) EnvKeys() []string {
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	return keys
}

// normalizeAccount checks what a client sent for the account fields and fixes the
// folder to an absolute path under the server user's home, creating it so the
// first sign-in has somewhere to write.
func normalizeAccount(cfg *ProviderConfig) error {
	kind, isCLI := CLIKindOf(cfg.Kind)
	if !isCLI {
		if cfg.ConfigDir != "" || len(cfg.Env) > 0 || cfg.Command != "" {
			return fmt.Errorf("account folder, command and environment only apply to coding-agent providers")
		}
		return nil
	}
	if kind == CLIKindACP && strings.TrimSpace(cfg.Command) == "" {
		return fmt.Errorf("a custom ACP agent needs a command")
	}
	if kind != CLIKindACP && (cfg.Command != "" || len(cfg.Args) > 0 || len(cfg.Env) > 0) {
		return fmt.Errorf("%s doesn't take a custom command or environment", cfg.Kind)
	}
	for k := range cfg.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || blockedEnv(k) {
			return fmt.Errorf("environment variable %q isn't allowed", k)
		}
	}
	if cfg.ConfigDir == "" {
		return nil
	}
	if configDirVar[kind] == "" {
		return fmt.Errorf("%s keeps one login per machine, so it has no account folder", cfg.Kind)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := cfg.ConfigDir
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("account folder must be an absolute path or start with ~/")
	}
	dir = filepath.Clean(dir)
	if rel, err := filepath.Rel(home, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("account folder must be inside your home folder")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating account folder: %w", err)
	}
	cfg.ConfigDir = dir
	return nil
}
