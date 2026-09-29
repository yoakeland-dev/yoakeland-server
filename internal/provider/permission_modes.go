package provider

import (
	"strings"
	"yoakeland/internal/agent"
)

// ModeSpec says what one yoakeland mode actually does on a provider
// (SPEC.md §4c, the `permissionModes` model capability). Names and
// descriptions belong to the client; this is the honest part: what runs
// without asking, and who enforces it.
type ModeSpec struct {
	Mode string `json:"mode"`
	// Policy maps read/edit/run/network to "allow", "ask" or "deny".
	Policy map[string]string `json:"policy"`
	// EnforcedBy is "agent" when the agent's own mode or sandbox backs the
	// policy, "yoakeland" when only yoakeland's permission bridge does.
	EnforcedBy string `json:"enforcedBy"`
	// Native is the agent's own name for it, when it has one.
	Native string `json:"native,omitempty"`
	// Sandbox: commands run confined to the project.
	Sandbox bool `json:"sandbox,omitempty"`
	// Plan, on the plan entry only: "native" (the agent's own plan mode),
	// "emulated" (a read-only turn told to plan) or "yoakeland" (the
	// write_plan tool).
	Plan string `json:"plan,omitempty"`
}

func pol(read, edit, run, net string) map[string]string {
	return map[string]string{"read": read, "edit": edit, "run": run, "network": net}
}

// The policies are the same on every provider; what differs is who enforces
// them and what the agent calls them.
var (
	polReadOnly    = pol("allow", "deny", "deny", "deny")
	polAsk         = pol("allow", "ask", "ask", "ask")
	polAcceptEdits = pol("allow", "allow", "ask", "ask")
	// "auto": the agent's own reviewer decides, asking only when it isn't sure.
	polAuto       = pol("allow", "allow", "auto", "auto")
	polAutonomous = pol("allow", "allow", "allow", "allow")
)

// modeSpecs lists the modes a provider offers; auto only where its agent has a reviewer.
func modeSpecs(by func(agent.Mode) (enforcedBy, native string), plan string, sandbox, auto bool) []ModeSpec {
	mk := func(m agent.Mode, p map[string]string) ModeSpec {
		by, native := by(m)
		s := ModeSpec{Mode: string(m), Policy: p, EnforcedBy: by, Native: native}
		if m == agent.ModeAutonomous {
			s.Sandbox = sandbox
		}
		if m == agent.ModePlan {
			s.Plan = plan
		}
		return s
	}
	specs := []ModeSpec{
		mk(agent.ModeReadOnly, polReadOnly),
		mk(agent.ModeReadWrite, polAsk),
		mk(agent.ModeAcceptEdits, polAcceptEdits),
	}
	if auto {
		specs = append(specs, mk(agent.ModeAuto, polAuto))
	}
	return append(specs, mk(agent.ModeAutonomous, polAutonomous), mk(agent.ModePlan, polReadOnly))
}

// apiModes: yoakeland runs the loop, so it enforces every mode itself.
func apiModes() []ModeSpec {
	return modeSpecs(func(agent.Mode) (string, string) { return "yoakeland", "" }, "yoakeland", true, false)
}

// claudeModes: auto only for a model that supports it (a discovered model says so).
func claudeModes(auto bool) []ModeSpec {
	native := map[agent.Mode]string{
		agent.ModeReadOnly: "plan", agent.ModeReadWrite: "default", agent.ModeAuto: "auto",
		agent.ModeAcceptEdits: "acceptEdits", agent.ModeAutonomous: "bypassPermissions", agent.ModePlan: "plan",
	}
	return modeSpecs(func(m agent.Mode) (string, string) { return "agent", native[m] }, "native", true, auto)
}

// codexModes: Codex has a sandbox and an approval policy, not modes. Edits
// are approved by yoakeland's bridge, since under `untrusted` Codex asks for
// them.
func codexModes() []ModeSpec {
	native := map[agent.Mode]string{
		agent.ModeReadOnly: "read-only", agent.ModeReadWrite: "workspace-write · untrusted",
		agent.ModeAcceptEdits: "workspace-write · untrusted", agent.ModeAutonomous: "workspace-write · never", agent.ModePlan: "read-only",
		agent.ModeAuto: "workspace-write · on-request · auto review",
	}
	return modeSpecs(func(m agent.Mode) (string, string) {
		if m == agent.ModeAcceptEdits {
			return "yoakeland", native[m]
		}
		return "agent", native[m]
	}, "emulated", true, true)
}

// acpModes maps what an ACP agent advertised: a mode it has a match for is
// enforced by the agent, the rest only by yoakeland's bridge.
func acpModes(choices []acpModeChoice) []ModeSpec {
	planNative := false
	specs := modeSpecs(func(m agent.Mode) (string, string) {
		if id, ok := pickACPMode(choices, m); ok {
			if m == agent.ModePlan {
				planNative = true
			}
			return "agent", id
		}
		return "yoakeland", ""
	}, "", false, false)
	for i := range specs {
		if specs[i].Mode == string(agent.ModePlan) {
			specs[i].Plan = "emulated"
			if planNative {
				specs[i].Plan = "native"
			}
		}
	}
	return specs
}

// withPermissionModesBy sets each model's own capability, for a provider whose
// models differ in what they support.
func withPermissionModesBy(models []ModelInfo, specs func(ModelInfo) []ModeSpec) []ModelInfo {
	out := make([]ModelInfo, len(models))
	for i, m := range models {
		out[i] = withPermissionModes([]ModelInfo{m}, specs(m))[0]
	}
	return out
}

// withPermissionModes sets the capability on every model, keeping what's there.
func withPermissionModes(models []ModelInfo, specs []ModeSpec) []ModelInfo {
	out := make([]ModelInfo, len(models)) // the manifest cache's slice isn't ours to edit
	for i, m := range models {
		caps := make(map[string]any, len(m.Capabilities)+1)
		for k, v := range m.Capabilities {
			caps[k] = v
		}
		caps["permissionModes"] = specs
		m.Capabilities = caps
		out[i] = m
	}
	return out
}

// acpModeChoice is one mode an ACP agent offers.
type acpModeChoice struct{ ID, Name string }

func normMode(s string) string {
	return strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(s))
}

// Ids are agent-defined, so a mode is matched by exact (normalized) name
// against the ones people actually use, never by substring — "auto" in
// particular means a classifier on some agents, not "bypass".
var acpModeNames = map[agent.Mode][]string{
	agent.ModePlan:        {"plan"},
	agent.ModeReadOnly:    {"ask", "readonly", "plan"},
	agent.ModeReadWrite:   {"default", "agent", "build"},
	agent.ModeAcceptEdits: {"acceptedits", "autoedit"},
	agent.ModeAutonomous:  {"yolo", "bypasspermissions", "bypass", "fullaccess", "alwaysapprove"},
}

// pickACPMode finds the agent's own mode for m, in the order of preference above.
func pickACPMode(choices []acpModeChoice, m agent.Mode) (string, bool) {
	for _, want := range acpModeNames[m] {
		for _, c := range choices {
			if normMode(c.ID) == want || normMode(c.Name) == want {
				return c.ID, true
			}
		}
	}
	return "", false
}

// planPrefix is the instruction a plan-mode prompt gets on an agent that
// plans by being told to.
func planPrefix(m agent.Mode) string {
	if m == agent.ModePlan {
		return agent.NativePlanInstruction
	}
	return ""
}
