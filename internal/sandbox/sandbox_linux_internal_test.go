//go:build linux

package sandbox

import "testing"

// TestLinuxSandbox_Mode_DemotesNamespaceOnlyToLocalRestricted guards against
// overclaiming isolation: the namespace-only fallback (no CLONE_NEWNET, no
// mount/root restriction) must be reported as "local_restricted_fallback",
// not "linux_namespaces", since it isolates the process tree but not the
// network or filesystem — a client relying on SPEC.md's guidance to warn
// only on local_restricted_fallback would otherwise show no warning for a
// project that is, in practice, running bash with full host network and
// filesystem access.
func TestLinuxSandbox_Mode_DemotesNamespaceOnlyToLocalRestricted(t *testing.T) {
	sb := &LinuxSandbox{mode: ModeNamespace}
	if got := sb.Mode(); got != ModeLocalRestr {
		t.Fatalf("Mode() = %q, want %q (namespace-only fallback must not be reported as genuinely isolated)", got, ModeLocalRestr)
	}
}

// TestLinuxSandbox_Mode_ReportsBubblewrapAndFallbackAsIs guards that the
// demotion above is narrowly scoped to ModeNamespace — bubblewrap (which
// does isolate network/filesystem via --unshare-net and an explicit bind
// allowlist) and the already-honest no-isolation fallback must be reported
// unchanged.
func TestLinuxSandbox_Mode_ReportsBubblewrapAndFallbackAsIs(t *testing.T) {
	if got := (&LinuxSandbox{mode: ModeBubblewrap}).Mode(); got != ModeBubblewrap {
		t.Errorf("Mode() = %q, want %q", got, ModeBubblewrap)
	}
	if got := (&LinuxSandbox{mode: ModeLocalRestr}).Mode(); got != ModeLocalRestr {
		t.Errorf("Mode() = %q, want %q", got, ModeLocalRestr)
	}
}
