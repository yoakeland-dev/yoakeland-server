package protocol

import "encoding/json"

// Model forwarding (SPEC.md "Model forwarding"): a chat on server B runs on a provider that lives
// on server A, with the model request relayed by a device connected to both. The device only
// passes these frames on.
const (
	// EventRelayOffer: a device tells a server which other servers it is connected to right now.
	EventRelayOffer EventType = "relay_offer"
	// EventForwardRequest carries one piece of a request body: B to the device, the device to A.
	EventForwardRequest EventType = "forward_request"
	// EventForwardCancel stops a request: B to the device, the device to A.
	EventForwardCancel EventType = "forward_cancel"
	// EventForwardChunk is one chunk of the answer: A to the device, the device to B.
	EventForwardChunk EventType = "forward_chunk"
	// EventForwardEnd closes a request, with the provider's error if it failed.
	EventForwardEnd EventType = "forward_end"
	// EventForwardState tells a chat's watchers what a forwarded turn is waiting for.
	EventForwardState EventType = "forward_state"
)

// FeatureForwarding is listed in ServerIdentityData.Features by a server that speaks the above.
const FeatureForwarding = "forwarding"

// ForwardPieceSize is the most a forward_request's data holds; a body is cut into pieces of at
// most this many bytes.
const ForwardPieceSize = 256 << 10

// MaxForwardBody is the largest request body a server will reassemble.
const MaxForwardBody = 64 << 20

// Forward states.
const (
	ForwardWaiting  = "waiting"  // no device connected to both servers is available yet
	ForwardRelaying = "relaying" // a device has the request
)

// RelayOfferData is the full set of other servers (by server id) the sending device is connected
// to, replacing its previous offer.
type RelayOfferData struct {
	Servers []string `json:"servers"`
}

// ForwardRequestData is one piece of a request body. RequestID is minted by B; ServerID is A's.
// Data is JSON text cut anywhere, Seq counts from 0 and Last marks the final piece.
type ForwardRequestData struct {
	RequestID string `json:"requestId"`
	ServerID  string `json:"serverId"`
	Seq       int    `json:"seq"`
	Last      bool   `json:"last"`
	Data      string `json:"data"`
}

type ForwardCancelData struct {
	RequestID string `json:"requestId"`
}

type ForwardChunkData struct {
	RequestID string       `json:"requestId"`
	Chunk     ForwardChunk `json:"chunk"`
}

type ForwardEndData struct {
	RequestID string `json:"requestId"`
	Error     string `json:"error,omitempty"`
}

type ForwardStateData struct {
	ServerID string `json:"serverId"`
	State    string `json:"state"`
}

// ForwardChunk is agent.ModelStreamChunk on the wire. Kind is the StreamEventKind.
type ForwardChunk struct {
	Kind         string           `json:"kind"`
	Delta        string           `json:"delta,omitempty"`
	ToolCall     *ForwardToolCall `json:"toolCall,omitempty"`
	Usage        *UsageData       `json:"usage,omitempty"`
	FinishReason string           `json:"finishReason,omitempty"`
	Raw          json.RawMessage  `json:"raw,omitempty"`
	Context      *ForwardContext  `json:"context,omitempty"`
}

// ForwardToolCall has the JSON shape of agent.ToolCall.
type ForwardToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	NativeToolInfo
}

// ForwardContext is agent.ContextSnapshot.
type ForwardContext struct {
	ContextTokens int    `json:"contextTokens,omitempty"`
	WindowTokens  int    `json:"windowTokens,omitempty"`
	Source        string `json:"source,omitempty"`
}
