package main

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"yoakeland/internal/protocol"
)

// readProcessTree returns root and all its descendants, ordered by pid.
// ponytail: Linux /proc only (empty elsewhere), assumes CLK_TCK=100; add
// sysctl/Toolhelp readers if a mac/windows host needs the page.
func readProcessTree(root int) []protocol.ProcessInfo {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	all := map[int]protocol.ProcessInfo{}
	kids := map[int][]int{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ok := readProc(pid); ok {
			all[pid] = p
			kids[p.PPID] = append(kids[p.PPID], pid)
		}
	}
	var out []protocol.ProcessInfo
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		p, ok := all[queue[0]]
		if !ok {
			continue
		}
		out = append(out, p)
		for _, c := range kids[p.PID] {
			// a reused pid can't be a child that started before its parent
			if all[c].StartTimeMs >= p.StartTimeMs {
				queue = append(queue, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func readProc(pid int) (protocol.ProcessInfo, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return protocol.ProcessInfo{}, false
	}
	// "pid (comm) S ppid ..." where comm may itself contain spaces and parens.
	s := string(b)
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < l || len(s) < r+3 {
		return protocol.ProcessInfo{}, false
	}
	f := strings.Fields(s[r+2:]) // f[0]=state f[1]=ppid f[11]=utime f[12]=stime f[19]=starttime f[21]=rss(pages)
	if len(f) < 22 {
		return protocol.ProcessInfo{}, false
	}
	n := func(i int) int64 { v, _ := strconv.ParseInt(f[i], 10, 64); return v }
	p := protocol.ProcessInfo{
		PID: pid, PPID: int(n(1)), Name: s[l+1 : r], Status: f[0],
		CPUTimeMs: (n(11) + n(12)) * 10, StartTimeMs: n(19) * 10, RSSBytes: uint64(n(21)) * uint64(os.Getpagesize()),
	}
	if c, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		p.Command = strings.TrimSpace(strings.ReplaceAll(string(c), "\x00", " "))
		if len(p.Command) > 2048 {
			p.Command = p.Command[:2048]
		}
	}
	return p, true
}

// handleSignalProcess answers signal_process. Local connections only: a TCP peer
// may not signal processes on the server's machine.
func handleSignalProcess(ipc *protocol.IPCStream, network string, env *protocol.Envelope) {
	if network == "tcp" {
		_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{
			Message: "signal_process isn't available from a paired device; do it on the computer running the server",
		})
		return
	}
	var data protocol.SignalProcessData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{Message: "malformed signal_process payload"})
		return
	}
	_ = ipc.SendEvent(protocol.EventProcessSignaled, "", "", "", signalProcess(os.Getpid(), data.PID, data.Signal))
}

// signalProcess sends sig to pid only if pid is a live descendant of root (the
// server). The tree is walked now, not cached, and root itself is refused.
// Pid reuse: the walk checks parent/child start times, and the target's start
// time is re-read just before the signal. A tiny window remains between that
// read and the kill; there is no cheaper exact guard.
func signalProcess(root, pid int, sig string) protocol.ProcessSignaledData {
	out := protocol.ProcessSignaledData{PID: pid, Signal: sig}
	if sig != "SIGINT" && sig != "SIGKILL" {
		out.Message = `signal must be "SIGINT" or "SIGKILL"`
		return out
	}
	if sig == "SIGINT" && runtime.GOOS == "windows" {
		out.Message = "SIGINT isn't supported on Windows"
		return out
	}
	if pid == root {
		out.Message = "pid is the server itself, not a live descendant"
		return out
	}
	var target protocol.ProcessInfo
	found := false
	for _, p := range readProcessTree(root) {
		if p.PID == pid {
			target, found = p, true
			break
		}
	}
	if !found {
		out.Message = "pid is not a live descendant of the server"
		return out
	}
	// Still the same process a moment later, or the pid was reused.
	if now, ok := readProc(pid); !ok || now.StartTimeMs != target.StartTimeMs {
		out.Message = "pid is not a live descendant of the server (it exited while being checked)"
		return out
	}
	if err := sendSignal(pid, sig); err != nil {
		out.Message = err.Error()
		return out
	}
	out.Signaled = true
	return out
}

func sendSignal(pid int, sig string) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	if sig == "SIGKILL" {
		return p.Kill()
	}
	if runtime.GOOS == "windows" {
		return errors.New("SIGINT isn't supported on Windows")
	}
	return p.Signal(syscall.SIGINT)
}
