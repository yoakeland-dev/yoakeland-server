# yoake shell integration — sourced via `bash --rcfile <this file>`
# (internal/terminal/shellintegration.go). --rcfile replaces bash's own
# ~/.bashrc lookup for an interactive shell, so we source it ourselves below
# rather than silently dropping the user's config.
#
# Emits a subset of VS Code's OSC 633 protocol on PROMPT_COMMAND: D;<exit
# code> (previous command finished), P;Cwd=<path> (cwd report), A (prompt
# start) — see yoake-gui's terminalShellIntegration.ts for the parser.
#
# ponytail: no command-start (C) marker here — bash's only hook for that is
# `trap ... DEBUG`, which also fires inside subshells and command
# substitution ($(...)), misattributing those as top-level commands.
# Prompt-boundary markers only (good enough for exit-code gutter marks + a
# live cwd); upgrade to a guarded DEBUG trap if per-command decorations are
# ever needed. zsh gets the full set below since its preexec/precmd hooks
# don't have this problem.

[ -n "$YOAKE_SHELL_INTEGRATION" ] && return
export YOAKE_SHELL_INTEGRATION=1

[ -r ~/.bashrc ] && source ~/.bashrc

__yoake_precmd() {
  # Must be the very first thing PROMPT_COMMAND runs (see the prepend below)
  # so $? is still the exiting command's own status, not ours.
  local ec=$?
  printf '\033]633;D;%s\007\033]633;P;Cwd=%s\007\033]633;A\007' "$ec" "$PWD"
}
PROMPT_COMMAND="__yoake_precmd${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
