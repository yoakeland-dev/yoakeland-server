package main

import (
	"fmt"
	"maps"
	"slices"
)

// Capabilities a person below the Full tier has or lacks (PLAN_PERMISSIONS "Caps"). Admin powers
// are never caps: they change with the tier.
const (
	CapFilesEdit           = "files_edit"
	CapCommandsSandboxed   = "commands_sandboxed"
	CapCommandsUnsandboxed = "commands_unsandboxed"
	CapAutonomous          = "autonomous"
	CapCLI                 = "cli"
	CapMCPTools            = "mcp_tools"
	CapBrowser             = "browser"
	CapTerminalsWatch      = "terminals_watch"
	CapProcesses           = "processes"
	CapOthersChats         = "others_chats"

	// capBrowserWatch is not a stored cap: the event table's name for browser being watch or use.
	capBrowserWatch = "browser_watch"
)

// Browser levels.
const (
	browserOff   = "off"
	browserWatch = "watch"
	browserUse   = "use"
)

// capNames is every stored cap, in the order clients list them.
var capNames = []string{CapFilesEdit, CapCommandsSandboxed, CapCommandsUnsandboxed, CapAutonomous, CapCLI, CapMCPTools, CapBrowser, CapTerminalsWatch, CapProcesses, CapOthersChats}

// Caps is a resolved set of capabilities.
type Caps struct {
	FilesEdit, CommandsSandboxed, CommandsUnsandboxed, Autonomous bool
	CLI                                                           []string // registered provider names
	AllCLI                                                        bool     // every provider (Full)
	MCPTools                                                      bool
	Browser                                                       string // off, watch or use
	TerminalsWatch, Processes, OthersChats                        bool
}

// allCaps is what the owner and Full people have.
func allCaps() Caps {
	return Caps{FilesEdit: true, CommandsSandboxed: true, CommandsUnsandboxed: true, Autonomous: true, AllCLI: true,
		MCPTools: true, Browser: browserUse, TerminalsWatch: true, Processes: true, OthersChats: true}
}

// tierDefaults are what Limited and Trusted people start with (the two are the same today; the
// Trusted tier's extra power is in the event table).
func tierDefaults(tier string) map[string]any {
	return map[string]any{
		CapFilesEdit: true, CapCommandsSandboxed: true, CapCommandsUnsandboxed: false, CapAutonomous: true,
		CapCLI: []string{}, CapMCPTools: false, CapBrowser: browserOff,
		CapTerminalsWatch: false, CapProcesses: true, CapOthersChats: false,
	}
}

// offValue is what a cap is set to when a device denies it.
func offValue(name string) any {
	switch name {
	case CapCLI:
		return []string{}
	case CapBrowser:
		return browserOff
	}
	return false
}

// capsFromMap reads a merged cap map; a value of the wrong type counts as off, so a malformed
// overlay can only take access away.
func capsFromMap(m map[string]any) Caps {
	b := func(k string) bool { v, _ := m[k].(bool); return v }
	c := Caps{FilesEdit: b(CapFilesEdit), CommandsSandboxed: b(CapCommandsSandboxed), CommandsUnsandboxed: b(CapCommandsUnsandboxed),
		Autonomous: b(CapAutonomous), MCPTools: b(CapMCPTools), TerminalsWatch: b(CapTerminalsWatch), Processes: b(CapProcesses),
		OthersChats: b(CapOthersChats), Browser: browserOff}
	switch br, _ := m[CapBrowser].(string); br {
	case browserWatch, browserUse:
		c.Browser = br
	}
	switch l := m[CapCLI].(type) {
	case []string:
		c.CLI = slices.Clone(l)
	case []any:
		for _, v := range l {
			if s, ok := v.(string); ok {
				c.CLI = append(c.CLI, s)
			}
		}
	}
	return c
}

// ToMap is the wire form of c. cliAll names every CLI provider, listed for a Caps that has them all.
func (c Caps) ToMap(cliAll []string) map[string]any {
	cli := c.CLI
	if c.AllCLI {
		cli = cliAll
	}
	if cli == nil {
		cli = []string{}
	}
	return map[string]any{
		CapFilesEdit: c.FilesEdit, CapCommandsSandboxed: c.CommandsSandboxed, CapCommandsUnsandboxed: c.CommandsUnsandboxed,
		CapAutonomous: c.Autonomous, CapCLI: cli, CapMCPTools: c.MCPTools, CapBrowser: c.Browser,
		CapTerminalsWatch: c.TerminalsWatch, CapProcesses: c.Processes, CapOthersChats: c.OthersChats,
	}
}

// AllowCLI reports whether the CLI agent registered as name may be used.
func (c Caps) AllowCLI(name string) bool { return c.AllCLI || slices.Contains(c.CLI, name) }

// Has reports whether the cap (or capBrowserWatch) is on.
func (c Caps) Has(name string) bool {
	switch name {
	case CapFilesEdit:
		return c.FilesEdit
	case CapCommandsSandboxed:
		return c.CommandsSandboxed
	case CapCommandsUnsandboxed:
		return c.CommandsUnsandboxed
	case CapAutonomous:
		return c.Autonomous
	case CapCLI:
		return c.AllCLI || len(c.CLI) > 0
	case CapMCPTools:
		return c.MCPTools
	case CapBrowser:
		return c.Browser == browserUse
	case capBrowserWatch:
		return c.Browser == browserWatch || c.Browser == browserUse
	case CapTerminalsWatch:
		return c.TerminalsWatch
	case CapProcesses:
		return c.Processes
	case CapOthersChats:
		return c.OthersChats
	}
	return false
}

// validateCaps checks a caps overlay a person sets: only known caps, with values of the right shape.
func validateCaps(m map[string]any) error {
	for k, v := range m {
		switch k {
		case CapCLI:
			switch l := v.(type) {
			case []string:
			case []any:
				for _, e := range l {
					if _, ok := e.(string); !ok {
						return fmt.Errorf("cap %s must be a list of provider names", k)
					}
				}
			default:
				return fmt.Errorf("cap %s must be a list of provider names", k)
			}
		case CapBrowser:
			if s, _ := v.(string); s != browserOff && s != browserWatch && s != browserUse {
				return fmt.Errorf("cap browser must be off, watch or use")
			}
		default:
			if !slices.Contains(capNames, k) {
				return fmt.Errorf("unknown capability %q", k)
			}
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("cap %s must be true or false", k)
			}
		}
	}
	return nil
}

// Access is what one connection may do, worked out from its credential on every request.
type Access struct {
	Tier   string
	Caps   Caps
	UserID string
	Local  bool // the local socket or -stdio: the owner, always full
}

// localAccess is the owner on the local socket.
func localAccess() Access { return Access{Tier: TierFull, Caps: allCaps(), UserID: ownerID, Local: true} }

// Access resolves who a connection is. The local socket and -stdio are the owner. A TCP peer
// takes its credential's person, held to the device's ceiling and denials; a credential or person
// that no longer exists is limited with every cap off.
func (s *ServerSettings) Access(network, credential string) Access {
	if network != "tcp" {
		return localAccess()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accessLocked(credential)
}

func (s *ServerSettings) accessLocked(credential string) Access {
	denied := Access{Tier: TierLimited, Caps: capsFromMap(nil)}
	rec, ok := s.Tokens[credential]
	if !ok {
		return denied
	}
	u, ok := s.resolveUserLocked(rec.UserID)
	if !ok {
		return denied
	}
	tier := minTier(u.Tier, rec.MaxTier)
	a := Access{Tier: tier, UserID: u.ID}
	if tier == TierFull {
		a.Caps = allCaps()
		return a
	}
	caps := tierDefaults(tier)
	if tier == TierLimited {
		maps.Copy(caps, s.LimitedDefaults)
	}
	maps.Copy(caps, u.Caps)
	for _, name := range rec.Deny {
		if _, known := caps[name]; known {
			caps[name] = offValue(name)
		}
	}
	a.Caps = capsFromMap(caps)
	return a
}

// AtLeast reports whether a is at tier t or above.
func (a Access) AtLeast(t string) bool { return tierRank(a.Tier) >= tierRank(t) }

// Full is the owner's tier.
func (a Access) Full() bool { return a.Tier == TierFull }

// accessFor is the access of the connection peerID on network. A peer that has gone is denied.
func (d connDeps) accessFor(network, peerID string) Access {
	if network != "tcp" {
		return localAccess()
	}
	p, ok := d.peers.Get(peerID)
	if !ok || p.TokenName == "" {
		return Access{Tier: TierLimited}
	}
	return d.settings.Access(network, p.TokenName)
}
