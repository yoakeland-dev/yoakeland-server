# yoake shell integration (zsh) — see integration.zshenv for why ZDOTDIR
# points here. YOAKE_ORIG_ZDOTDIR carries the real ZDOTDIR (or empty, for
# $HOME) through so the user's own ~/.zshrc, when it has a real ZDOTDIR,
# still gets sourced below rather than replaced.
[ -n "$YOAKE_SHELL_INTEGRATION" ] && return
export YOAKE_SHELL_INTEGRATION=1

autoload -Uz add-zsh-hook

# Emits VS Code's OSC 633 A/B/C/D/E/P subset — see yoake-gui's
# terminalShellIntegration.ts for the parser. Registered *before* the user's
# own ~/.zshrc runs below, so __yoake_precmd is first in zsh's
# precmd_functions array and sees $? exactly as the last command left it —
# a precmd hook installed by the user's own prompt framework afterwards
# can't have run anything yet to clobber it.
__yoake_precmd() {
  printf '\033]633;D;%s;%s\007\033]633;P;Cwd=%s\007\033]633;A;%s\007' "$?" "$__yoake_nonce" "$PWD" "$__yoake_nonce"
}
__yoake_preexec() {
  # E carries the command line; control chars would end the OSC early.
  printf '\033]633;B\007\033]633;E;%s\007\033]633;C;%s\007' "${1//[[:cntrl:]]/ }" "$__yoake_nonce"
}
add-zsh-hook precmd __yoake_precmd
add-zsh-hook preexec __yoake_preexec

if [ -n "$YOAKE_ORIG_ZDOTDIR" ]; then
  ZDOTDIR="$YOAKE_ORIG_ZDOTDIR"
else
  unset ZDOTDIR
fi
# With no real ZDOTDIR, $HOME is the project root: its .zshrc is the repo's
# own file, so nothing more is sourced.
if [ -n "$ZDOTDIR" ] && [ -f "$ZDOTDIR/.zshrc" ]; then
  source "$ZDOTDIR/.zshrc"
fi
