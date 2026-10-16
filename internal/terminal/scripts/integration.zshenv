# yoake shell integration (zsh) — ZDOTDIR is pointed at this script's own
# directory (internal/terminal/shellintegration.go) so zsh sources this
# instead of the user's real ~/.zshenv. zsh reads .zshenv before anything
# else, from whatever ZDOTDIR is at that moment — so unlike .zshrc (see
# integration.zshrc) there's no later point to restore the real ZDOTDIR
# before sourcing; just chain straight through. Only the real ZDOTDIR is
# chained to: with none set, $HOME is the project root, and its .zshenv is
# the repo's own file.
# The mark nonce (see integration.bash) is taken here, before anything else
# runs, since zsh reads this file first and every later file is in this shell.
__yoake_nonce=$YOAKE_MARK_NONCE
unset YOAKE_MARK_NONCE
if [ -n "$YOAKE_ORIG_ZDOTDIR" ] && [ -f "$YOAKE_ORIG_ZDOTDIR/.zshenv" ]; then
  source "$YOAKE_ORIG_ZDOTDIR/.zshenv"
fi
