package agent

import (
	"encoding/json"
)

// A raw JSON field on the way to disk (ToolCall.Args, Message.RawContent) is
// copied into the session file verbatim, and encoding/json validates every
// byte of it. A value that isn't exactly *one* JSON value — a model stream
// that repeated or merged its tool-call fragments leaves {"a":1},{"b":2} in a
// buffer — therefore fails the whole MarshalIndent, costing the entire
// conversation rather than the one field, and (because the value stays in
// memory) every later save of that session too. These helpers keep that from
// happening: saveableToolCalls/wireArgs where a call is stored or sent, and
// repairSessionState as the store's last line of defence.

// rawArgsLimit bounds how much of an unusable argument blob the stub keeps.
const rawArgsLimit = 4000

// saveableRaw reports whether raw can be marshaled: nil is fine (it becomes
// null), anything else must be one valid JSON value.
func saveableRaw(raw json.RawMessage) bool {
	return raw == nil || json.Valid(raw)
}

// invalidRawStub stands in for arguments that aren't valid JSON, keeping the
// model's own text so the failure is visible in the chat and in the file.
func invalidRawStub(raw json.RawMessage) json.RawMessage {
	stub, err := json.Marshal(struct {
		Error string `json:"error"`
		Raw   string `json:"raw"`
	}{"the model sent arguments that are not valid JSON", truncateRunes(string(raw), rawArgsLimit)})
	if err != nil {
		return json.RawMessage(`{"error":"the model sent arguments that are not valid JSON"}`)
	}
	return stub
}

// sanitizeToolCallArgs replaces a tool call's arguments in place when they
// aren't one JSON value, reporting whether it did. The store's repair pass
// uses it; the turn loop stores repaired copies instead (saveableToolCalls),
// so the call itself still runs against what the model actually sent.
func sanitizeToolCallArgs(tc *ToolCall) bool {
	if saveableRaw(tc.Args) {
		return false
	}
	tc.Args = invalidRawStub(tc.Args)
	return true
}

// wireArgs is a tool call's arguments as a client may be sent them: the wire
// carries them as raw JSON in tool_call_requested, so the same unusable value
// that breaks a save breaks that frame.
func wireArgs(args json.RawMessage) json.RawMessage {
	if saveableRaw(args) {
		return args
	}
	return invalidRawStub(args)
}

// saveableToolCalls is how tool calls go into history: the model's own bytes
// unless they aren't one JSON value, in which case a stub. The call itself is
// still run with its real arguments — a tool's own error on unparseable input
// is the signal SPEC.md §3.2 relies on — only what gets stored is repaired.
// Returns calls unchanged when nothing needs replacing.
func saveableToolCalls(calls []ToolCall) []ToolCall {
	var out []ToolCall
	for i, tc := range calls {
		if saveableRaw(tc.Args) {
			continue
		}
		if out == nil {
			out = append([]ToolCall(nil), calls...)
		}
		out[i].Args = invalidRawStub(tc.Args)
	}
	if out == nil {
		return calls
	}
	return out
}

// repairSessionState returns a copy of state in which every raw JSON field can
// be marshaled, plus how many values it had to replace. A copy because the
// live history belongs to the Agent: the store must not reach into it from
// under its own lock, and the turn in flight may still be appending to it.
func repairSessionState(state *SessionState) (*SessionState, int) {
	out := *state
	fixed := 0
	out.History, fixed = repairedMessages(state.History)
	if len(state.Branches) > 0 {
		branches := make(map[string][][]Message, len(state.Branches))
		for point, alts := range state.Branches {
			cp := make([][]Message, len(alts))
			for i, alt := range alts {
				var n int
				cp[i], n = repairedMessages(alt)
				fixed += n
			}
			branches[point] = cp
		}
		out.Branches = branches
	}
	return &out, fixed
}

// repairedMessages is repairSessionState for one message list, following
// subagent transcripts since they are saved inside their parent message.
func repairedMessages(in []Message) ([]Message, int) {
	if in == nil {
		return nil, 0
	}
	out := append([]Message(nil), in...)
	fixed := 0
	for i := range out {
		m := &out[i]
		if !saveableRaw(m.RawContent) {
			// Only a replay cache for the provider's own blocks: dropping it
			// costs a verbatim replay, not the message's text or tool calls.
			m.RawContent = nil
			fixed++
		}
		if len(m.ToolCalls) > 0 {
			calls := append([]ToolCall(nil), m.ToolCalls...)
			for j := range calls {
				if sanitizeToolCallArgs(&calls[j]) {
					fixed++
				}
			}
			m.ToolCalls = calls
		}
		if len(m.Subagent) > 0 {
			var n int
			m.Subagent, n = repairedMessages(m.Subagent)
			fixed += n
		}
	}
	return out, fixed
}
