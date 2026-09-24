package main

import (
	"bufio"
	"bytes"
	"context"
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

// startGitWatcher returns nil for a project that isn't a git repo (or when
// git isn't installed) — such projects get no git_status at all.
func startGitWatcher(root string, publish func(protocol.GitStatusData)) *gitWatcher {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir").Output()
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
	out, err := exec.Command("git", "--no-optional-locks", "-C", w.root, "status", "--porcelain=v2", "--branch").Output()
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

// parseGitFile reads one porcelain-v2 entry line. Paths may contain spaces,
// so the path is everything after the fixed field count; renames/copies
// append "\t<origPath>", which is dropped.
func parseGitFile(line string) (protocol.GitFile, bool) {
	var xy, path string
	switch line[0] {
	case '?':
		return protocol.GitFile{Path: line[2:], Status: "?"}, true
	case '1':
		if f := strings.SplitN(line, " ", 9); len(f) == 9 {
			xy, path = f[1], f[8]
		}
	case '2':
		if f := strings.SplitN(line, " ", 10); len(f) == 10 {
			xy = f[1]
			path, _, _ = strings.Cut(f[9], "\t")
		}
	case 'u':
		if f := strings.SplitN(line, " ", 11); len(f) == 11 {
			return protocol.GitFile{Path: f[10], Status: "U"}, true
		}
	}
	if len(xy) != 2 || path == "" {
		return protocol.GitFile{}, false
	}
	// The worktree side is what's most recent; fall back to the staged side.
	s := xy[1]
	if s == '.' {
		s = xy[0]
	}
	return protocol.GitFile{Path: path, Status: string(s)}, true
}

func gitStatusEqual(a, b protocol.GitStatusData) bool {
	return a.ProjectPath == b.ProjectPath && a.Detached == b.Detached && a.DirtyFiles == b.DirtyFiles &&
		a.Ahead == b.Ahead && a.Behind == b.Behind && a.HasUpstream == b.HasUpstream &&
		(a.Branch == nil) == (b.Branch == nil) && (a.Branch == nil || *a.Branch == *b.Branch) &&
		slices.Equal(a.Files, b.Files)
}
