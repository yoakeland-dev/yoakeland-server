# yoake shell integration (zsh) — ZDOTDIR is pointed at this script's own
# directory (internal/terminal/shellintegration.go) so zsh sources this
# instead of the user's real ~/.zshenv. zsh reads .zshenv before anything
# else, from whatever ZDOTDIR is at that moment — so unlike .zshrc (see
# integration.zshrc) there's no later point to restore the real ZDOTDIR
# before sourcing; just chain straight through.
if [ -n "$YOAKE_ORIG_ZDOTDIR" ] && [ -f "$YOAKE_ORIG_ZDOTDIR/.zshenv" ]; then
  source "$YOAKE_ORIG_ZDOTDIR/.zshenv"
elif [ -z "$YOAKE_ORIG_ZDOTDIR" ] && [ -f "$HOME/.zshenv" ]; then
  source "$HOME/.zshenv"
fi
