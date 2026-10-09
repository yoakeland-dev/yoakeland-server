package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/protocol"
)

// gitWatcher keeps one project's git_status (SPEC.md §3.2) current. It
// re-runs `git status` when something under .git that tracks branch/ahead/
// behind/index state changes, or when Poke is called (after a turn, since
// edits to the working tree don't touch .git at all), and publishes only
// when the result differs from the last one. Checks happen on a 1s tick,
// which is also the rate limit.
//
// ponytail: mtime polling of a handful of paths instead of an fsnotify
// dependency — one stat per path per second. Working-tree edits made
// outside a turn (the user's own editor) aren't seen until the next turn,
// HEAD move, or index write; watch the tree with fsnotify if that matters.
type gitWatcher struct {
	root    string
	gitDir  string
	publish func(protocol.GitStatusData)
	cancel  context.CancelFunc

	mu     sync.Mutex
	last   *protocol.GitStatusData
	poked  bool
	finger string
}

// hostGit runs git outside the sandbox, so it must not run anything the
// project's own .git/config names: the agent can write that file from inside
// the sandbox, and core.fsmonitor alone is a shell command git status runs.
// Same set Claude Code and Codex landed on for this bug class: blank
// fsmonitor and hooks on every call, and safe.bareRepository=explicit (git
// 2.38) so a bare repo committed inside the tree is never picked up as the
// repository. -c reaches submodule git processes too (GIT_CONFIG_PARAMETERS).
// ponytail: filter.*.clean drivers still run; the sandbox keeps .git/config
// and submodule configs read-only so the agent can't add one; blank
// filter.*.clean via -c too if a global/system config ever defines one.
func hostGit(args ...string) *exec.Cmd {
	return exec.Command("git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "safe.bareRepository=explicit"}, args...)...)
}

// startGitWatcher returns nil for a project that isn't a git repo (or when
// git isn't installed) — such projects get no git_status at all.
func startGitWatcher(root string, publish func(protocol.GitStatusData)) *gitWatcher {
	out, err := hostGit("-C", root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &gitWatcher{root: root, gitDir: strings.TrimSpace(string(out)), publish: publish, cancel: cancel}
	w.finger = w.fingerprint()
	w.refresh()
	go w.loop(ctx)
	return w
}

func (w *gitWatcher) Stop() {
	if w != nil {
		w.cancel()
	}
}

// Poke asks for a recheck on the next tick.
func (w *gitWatcher) Poke() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.poked = true
	w.mu.Unlock()
}

// Last is the most recent status, or nil.
func (w *gitWatcher) Last() *protocol.GitStatusData {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func (w *gitWatcher) loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f := w.fingerprint()
		w.mu.Lock()
		due := w.poked || f != w.finger
		w.poked, w.finger = false, f
		w.mu.Unlock()
		if due {
			w.refresh()
		}
	}
}

// fingerprint is the mtimes of what moves when branch, index, or
// ahead/behind change: HEAD (checkout), index (add/commit), logs/HEAD (any
// HEAD movement), FETCH_HEAD (fetch), packed-refs, and the remote-tracking
// ref dirs (a push rewrites a loose ref there, which bumps its dir's mtime).
func (w *gitWatcher) fingerprint() string {
	var sb strings.Builder
	stat := func(p string) {
		if fi, err := os.Stat(p); err == nil {
			sb.WriteString(strconv.FormatInt(fi.ModTime().UnixNano(), 36))
		}
		sb.WriteByte('|')
	}
	for _, p := range []string{"HEAD", "index", "logs/HEAD", "FETCH_HEAD", "packed-refs"} {
		stat(filepath.Join(w.gitDir, p))
	}
	_ = filepath.WalkDir(filepath.Join(w.gitDir, "refs", "remotes"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			stat(p)
		}
		return nil
	})
	return sb.String()
}

func (w *gitWatcher) refresh() {
	// --no-optional-locks: plain `git status` may rewrite the index to
	// refresh stat info, which would bump index's mtime and retrigger this
	// watcher every second forever.
	out, err := hostGit("--no-optional-locks", "-c", "core.quotePath=false", "-C", w.root, "status", "--porcelain=v2", "--branch").Output()
	if err != nil {
		return
	}
	st := parseGitStatus(out)
	st.ProjectPath = w.root
	w.mu.Lock()
	changed := w.last == nil || !gitStatusEqual(*w.last, st)
	if changed {
		w.last = &st
	}
	w.mu.Unlock()
	if changed {
		w.publish(st)
	}
}

// parseGitStatus reads `git status --porcelain=v2 --branch`: "# branch.*"
// headers, then one line per changed/untracked/unmerged path.
func parseGitStatus(out []byte) protocol.GitStatusData {
	var st protocol.GitStatusData
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			if head := strings.TrimPrefix(line, "# branch.head "); head == "(detached)" {
				st.Detached = true
			} else {
				st.Branch = &head
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			st.HasUpstream = true
		case strings.HasPrefix(line, "# branch.ab "):
			// "# branch.ab +3 -1"
			f := strings.Fields(line)
			if len(f) == 4 {
				st.Ahead, _ = strconv.Atoi(strings.TrimPrefix(f[2], "+"))
				st.Behind, _ = strconv.Atoi(strings.TrimPrefix(f[3], "-"))
			}
		case strings.HasPrefix(line, "#"), line == "":
		default:
			st.DirtyFiles++
			if len(st.Files) < protocol.MaxGitFiles {
				if f, ok := parseGitFile(line); ok {
					st.Files = append(st.Files, f)
				}
			}
		}
	}
	return st
}

// parseGitEntry reads one porcelain-v2 entry line into its path, a
// rename/copy's source, and the two-letter XY code ("." = that side is
// unchanged). Paths may contain spaces, so the path is everything after the
// fixed field count.
func parseGitEntry(line string) (path, orig, xy string, ok bool) {
	switch line[0] {
	case '?':
		return line[2:], "", "??", true
	case '1':
		if f := strings.SplitN(line, " ", 9); len(f) == 9 {
			return f[8], "", f[1], len(f[1]) == 2
		}
	case '2':
		if f := strings.SplitN(line, " ", 10); len(f) == 10 {
			path, orig, _ = strings.Cut(f[9], "\t")
			return path, orig, f[1], len(f[1]) == 2
		}
	case 'u':
		if f := strings.SplitN(line, " ", 11); len(f) == 11 {
			return f[10], "", f[1], len(f[1]) == 2
		}
	}
	return "", "", "", false
}

// parseGitFile collapses an entry to one status letter for git_status.
func parseGitFile(line string) (protocol.GitFile, bool) {
	path, _, xy, ok := parseGitEntry(line)
	if !ok || path == "" {
		return protocol.GitFile{}, false
	}
	if line[0] == 'u' {
		return protocol.GitFile{Path: path, Status: "U"}, true
	}
	// The worktree side is what's most recent; fall back to the staged side.
	s := xy[1]
	if s == '.' {
		s = xy[0]
	}
	return protocol.GitFile{Path: path, Status: string(s)}, true
}

// maxGitChanges caps list_git_changes, which (unlike git_status) lists
// every file inside untracked folders.
const maxGitChanges = 5000

// gitChanges is list_git_changes: every changed path with both its staged
// and worktree state.
func gitChanges(root string) (files []protocol.GitChange, truncated bool, err error) {
	out, err := hostGit("--no-optional-locks", "-c", "core.quotePath=false", "-C", root, "status", "--porcelain=v2", "--untracked-files=all").Output()
	if err != nil {
		// git's own message ("not a git repository", …) beats a bare exit status.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, false, fmt.Errorf("git status: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, false, fmt.Errorf("git status: %w", err)
	}
	files = []protocol.GitChange{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		path, orig, xy, ok := parseGitEntry(line)
		if !ok {
			continue
		}
		if len(files) == maxGitChanges {
			return files, true, nil
		}
		files = append(files, protocol.GitChange{Path: path, OrigPath: orig, Index: xy[:1], Worktree: xy[1:], Conflict: line[0] == 'u'})
	}
	return files, false, nil
}

// gitShowHead returns rel's content at HEAD; missing is true when HEAD has
// no such file (new, untracked, or no commits yet).
func gitShowHead(root, rel string) (content []byte, missing bool, err error) {
	out, err := hostGit("-C", root, "show", "HEAD:"+rel).Output()
	if err != nil {
		if _, isExit := err.(*exec.ExitError); isExit {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("git show: %w", err)
	}
	return out, false, nil
}

func gitStatusEqual(a, b protocol.GitStatusData) bool {
	return a.ProjectPath == b.ProjectPath && a.Detached == b.Detached && a.DirtyFiles == b.DirtyFiles &&
		a.Ahead == b.Ahead && a.Behind == b.Behind && a.HasUpstream == b.HasUpstream &&
		(a.Branch == nil) == (b.Branch == nil) && (a.Branch == nil || *a.Branch == *b.Branch) &&
		slices.Equal(a.Files, b.Files)
}
