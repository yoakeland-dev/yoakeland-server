# yoake shell integration — sourced via `bash --rcfile <this file>`
# (internal/terminal/shellintegration.go). --rcfile replaces bash's own
# ~/.bashrc lookup for an interactive shell. It is not sourced again here:
# the terminal's HOME is the project root, so ~/.bashrc is the repo's own file.
#
# Emits a subset of VS Code's OSC 633 protocol on PROMPT_COMMAND: D;<exit
# code> (previous command finished), P;Cwd=<path> (cwd report), A (prompt
# start) — see yoake-gui's terminalShellIntegration.ts for the parser.
#
# Command start (C) comes from PS0 (bash 4.4+), which bash prints after
# reading a command line and before running it, and never for an empty
# line. Not a DEBUG trap: that also fires inside subshells and $(...).
# ponytail: no command-line (E) marker, PS0 can't see the line reliably
# (history is skipped under HISTCONTROL=ignorespace). zsh sends it; read it
# from `history 1` here if the client ever needs the command text in bash.

[ -n "$YOAKE_SHELL_INTEGRATION" ] && return
export YOAKE_SHELL_INTEGRATION=1
# Every mark carries this nonce, and scanMarks ignores unstamped ones, so a
# program's own output can't pass for a prompt or a command boundary. Unset,
# so the commands run here don't inherit it.
__yoake_nonce=$YOAKE_MARK_NONCE
unset YOAKE_MARK_NONCE


__yoake_precmd() {
  # Must be the very first thing PROMPT_COMMAND runs (see the prepend below)
  # so $? is still the exiting command's own status, not ours.
  local ec=$?
  printf '\033]633;D;%s;%s\007\033]633;P;Cwd=%s\007\033]633;A;%s\007' "$ec" "$__yoake_nonce" "$PWD" "$__yoake_nonce"
}
PROMPT_COMMAND="__yoake_precmd${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
PS0="${PS0}"$'\e]633;C;'"$__yoake_nonce"$'\a'
