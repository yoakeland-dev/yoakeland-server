# yoake shell integration — sourced via `bash --rcfile <this file>`
# (internal/terminal/shellintegration.go). --rcfile replaces bash's own
# ~/.bashrc lookup for an interactive shell, so we source it ourselves below
# rather than silently dropping the user's config.
#
# Emits a subset of VS Code's OSC 633 protocol on PROMPT_COMMAND: D;<exit
# code> (previous command finished), P;Cwd=<path> (cwd report), A (prompt
# start) — see yoake-gui's terminalShellIntegration.ts for the parser.
#
# Command start (C) comes from PS0 (bash 4.4+), which bash prints after
# reading a command line and before running it, and never for an empty
# line. Not a DEBUG trap: that also fires inside subshells and $(...).
# ponytail: no command-line (E) marker, PS0 can't see the line reliably
# (history is skipped under HISTCONTROL=ignorespace). zsh sends it.

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
PS0="${PS0}"$'\e]633;C\a'
