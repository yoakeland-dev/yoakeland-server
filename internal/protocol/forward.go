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
	// EventForwardGrant: a person lets another server use one of their providers here, as them.
	EventForwardGrant EventType = "forward_grant"
	// EventForwardRevoke takes such a grant back.
	EventForwardRevoke EventType = "forward_revoke"
	// EventListForwardGrants asks for the sender's grants; every one of the three is answered with EventForwardGrants.
	EventListForwardGrants EventType = "list_forward_grants"
	// EventForwardGrants is the sender's grants, in full.
	EventForwardGrants EventType = "forward_grants"
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
	// From is the server the request came from, stamped by the device (whatever B put there is
	// overwritten). A runs the request only if its person lent the provider to that server.
	From string `json:"from,omitempty"`
	// FromKey is what the device itself verified about its connection to From (the CA it pinned,
	// the DTLS fingerprint, the TLS host name), also stamped by the device. A server can claim any
	// id but can't fake this, so a grant holds the keys it was made for and A matches both.
	FromKey string `json:"fromKey,omitempty"`
	Seq     int    `json:"seq"`
	Last    bool   `json:"last"`
	Data    string `json:"data"`
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

// ForwardGrantData lets server ServerID use ProviderName on this server as the sender. ServerName
// is only a label to show in lists. ServerKey (sent with a grant) is the granting device's key
// for that server, as in ForwardRequestData.FromKey; ServerKeys (in a list) are every key it was
// granted for.
type ForwardGrantData struct {
	ServerID     string   `json:"serverId"`
	ServerName   string   `json:"serverName,omitempty"`
	ProviderName string   `json:"providerName"`
	ServerKey    string   `json:"serverKey,omitempty"`
	ServerKeys   []string `json:"serverKeys,omitempty"`
}

type ForwardRevokeData struct {
	ServerID     string `json:"serverId"`
	ProviderName string `json:"providerName"`
}

// ForwardGrantsData is one person's grants, sorted by server name then provider name.
type ForwardGrantsData struct {
	Grants []ForwardGrantData `json:"grants"`
}
