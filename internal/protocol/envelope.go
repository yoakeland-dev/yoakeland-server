package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
	"yoakeland/internal/diff"
)

// MaxFrameSize is the largest single newline-delimited frame the IPC
// framing (SPEC.md §2) accepts, in either direction — also reused by
// anything else that needs to cap a read against the same limit (e.g.
// tool.FileReadTool) rather than inventing a second number.
const MaxFrameSize = 10 * 1024 * 1024

type EventType string

const (
	EventUserPrompt           EventType = "user_prompt"
	EventPermissionResponse   EventType = "permission_response"
	EventCancelTurn           EventType = "cancel_turn"
	EventRestoreSnapshot      EventType = "restore_snapshot"
	EventAuth                 EventType = "auth"
	EventRegisterProvider     EventType = "register_provider"
	EventListProviders        EventType = "list_providers"
	EventSetProvider          EventType = "set_provider"
	EventSetModelOptions      EventType = "set_model_options"
	EventOAuthStart           EventType = "openrouter_oauth_start"
	EventOAuthComplete        EventType = "openrouter_oauth_complete"
	EventRemoveProvider       EventType = "remove_provider"
	EventListModels           EventType = "list_models"
	EventSetFavoriteModels    EventType = "set_favorite_models"
	EventListSessions         EventType = "list_sessions"
	EventDetectCLI            EventType = "detect_cli"
	EventPing                 EventType = "ping"
	EventOpenProject          EventType = "open_project"
	EventListProjects         EventType = "list_projects"
	EventRemoveProject        EventType = "remove_project"
	EventListFiles            EventType = "list_files"
	EventSetPlanMode          EventType = "set_plan_mode"
	EventGetSessionHistory    EventType = "get_session_history"
	EventSetSessionTitle      EventType = "set_session_title"
	EventSetSessionArchived   EventType = "set_session_archived"
	EventDeleteSession        EventType = "delete_session"
	EventSetSessionLayout     EventType = "set_session_layout"
	EventListDirectory        EventType = "list_directory"
	EventListSnapshots        EventType = "list_snapshots"
	EventSetPeerName          EventType = "set_peer_name"
	EventSetPeerAvatar        EventType = "set_peer_avatar"
	EventDisconnectPeer       EventType = "disconnect_peer"
	EventSetRemoteAccess      EventType = "set_remote_access"
	EventSetRemotePassword    EventType = "set_remote_password"
	EventSetServerIdentity    EventType = "set_server_identity"
	EventSetAutoGenerateTitle EventType = "set_auto_generate_title"
	EventRegisterToken        EventType = "register_token"
	EventListTokens           EventType = "list_tokens"
	EventRemoveToken          EventType = "remove_token"

	EventModelDelta         EventType = "model_delta"
	EventToolCallRequested  EventType = "tool_call_requested"
	EventPermissionRequest  EventType = "permission_request"
	EventToolResult         EventType = "tool_result"
	EventTurnCompleted      EventType = "turn_completed"
	EventSnapshotCapturing  EventType = "snapshot_capturing"
	EventSnapshotCreated    EventType = "snapshot_created"
	EventSnapshotRestored   EventType = "snapshot_restored"
	EventError              EventType = "error"
	EventSessionTakenOver   EventType = "session_taken_over"
	EventProviderRegistered EventType = "provider_registered"
	EventProviderList       EventType = "provider_list"
	EventProviderSet        EventType = "provider_set"
	EventModelOptionsSet    EventType = "model_options_set"
	EventOAuthURL           EventType = "openrouter_oauth_url"
	EventProviderRemoved    EventType = "provider_removed"
	EventModelList          EventType = "model_list"
	EventFavoriteModelsSet  EventType = "favorite_models_set"
	EventSessionList        EventType = "session_list"
	EventSessionUpdated     EventType = "session_updated"
	EventSessionDeleted     EventType = "session_deleted"
	EventDirectoryList      EventType = "directory_list"
	EventSnapshotList       EventType = "snapshot_list"
	EventCLIAvailability    EventType = "cli_availability"
	EventPong               EventType = "pong"
	EventProjectOpened      EventType = "project_opened"
	EventProjectList        EventType = "project_list"
	EventProjectRemoved     EventType = "project_removed"
	EventFileList           EventType = "file_list"
	EventPlanModeSet        EventType = "plan_mode_set"
	EventSessionHistory     EventType = "session_history"
	EventPeerList           EventType = "peer_list"
	EventRemoteAccessSet    EventType = "remote_access_set"
	EventRemotePasswordSet  EventType = "remote_password_set"
	EventServerIdentity     EventType = "server_identity"
	EventAutoGenerateTitle  EventType = "auto_generate_title"
	EventTokenRegistered    EventType = "token_registered"
	EventTokenList          EventType = "token_list"
	EventTokenRemoved       EventType = "token_removed"
)

type Envelope struct {
	Type       EventType       `json:"type"`
	SessionID  string          `json:"sessionId,omitempty"`
	TurnID     string          `json:"turnId,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// AuthData is the required first frame on a TCP connection (§2a), carrying
// the shared token, and an optional first frame on a unix connection that
// wants to self-report identity (Token is ignored there — file permissions
// are already the auth boundary). Name/DeviceType/Avatar are how a peer
// identifies itself for the peer registry (see PeerInfo) — the server can't
// infer "web vs desktop" from a raw socket, so the client says so.
type AuthData struct {
	Token      string `json:"token"`
	Name       string `json:"name,omitempty"`
	DeviceType string `json:"deviceType,omitempty"` // "web" | "desktop" | "server"
	// Avatar is a small image as a data URL (e.g. "data:image/png;base64,...")
	// — the client is expected to scale it down (96x96 is the convention)
	// before sending, since this travels in every peer_list push to every
	// peer. Oversized values are dropped rather than failing the connection
	// — see maxAvatarLen.
	Avatar string `json:"avatar,omitempty"`
}

// RegisterProviderData registers (or updates) a named provider config: a
// credential, not a model choice. One "anthropic" or "openrouter" provider
// covers every model that key can call — see ListModels/EventListModels to
// enumerate them, and SetProviderData to pick one for a session.
// APIKey is only meaningful for kind "api_key"; CLI-subprocess kinds
// ("cli_claude", "cli_codex") ignore it since they hold no credential at all.
type RegisterProviderData struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	HTTPProvider string `json:"httpProvider,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
	AuthMethod   string `json:"authMethod,omitempty"`
}

// ProviderSummary describes a registered provider without its credential.
type ProviderSummary struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	HTTPProvider   string   `json:"httpProvider,omitempty"`
	AuthMethod     string   `json:"authMethod,omitempty"`
	FavoriteModels []string `json:"favoriteModels,omitempty"`
}

type ProviderListData struct {
	Providers []ProviderSummary `json:"providers"`
}

type ProviderRegisteredData struct {
	Name string `json:"name"`
}

// ListModelsData requests the models available through one registered
// provider (credential). Kinds cli_claude/cli_codex return a fixed model
// list since there's no key to query an API with.
type ListModelsData struct {
	ProviderName string `json:"providerName"`
}

// ModelSummary is one model available through a provider's credential.
type ModelSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	IsFavorite  bool   `json:"isFavorite,omitempty"`
	// IsLegacy marks a cli_claude model T3's manifest still serves but no
	// longer lists as current — see provider.ModelInfo.IsLegacy.
	IsLegacy bool `json:"isLegacy,omitempty"`
	// Capabilities mirrors the upstream provider's own shape (OpenRouter's
	// reasoning/pricing/context_length fields, a hardcoded table for OpenAI)
	// rather than one unified schema — see provider.ModelInfo.Capabilities.
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

type ModelListData struct {
	ProviderName string         `json:"providerName"`
	Models       []ModelSummary `json:"models"`
}

// SetFavoriteModelsData replaces the favorite-model list for a provider.
// Server-persisted (ProviderConfig.FavoriteModels) so it follows the user
// across clients/devices instead of living in local frontend storage.
type SetFavoriteModelsData struct {
	ProviderName string   `json:"providerName"`
	Models       []string `json:"models"`
}

type FavoriteModelsSetData struct {
	ProviderName string   `json:"providerName"`
	Models       []string `json:"models"`
}

// SetProviderData hot-swaps the provider+model a session uses on its next
// turn. Model is required for kind "api_key" providers (pick one from
// ListModels); CLI-subprocess kinds fall back to their tool's own default
// when omitted.
type SetProviderData struct {
	ProviderName string `json:"providerName"`
	Model        string `json:"model,omitempty"`

	// ProjectPath resolves this session against the project its turns will
	// actually run in, binding it if it wasn't bound yet — same rules as
	// user_prompt (SPEC.md §4b). A client configuring a session it hasn't
	// prompted yet (a brand-new chat) must send it: project session
	// registries are separate, so setting a provider against the
	// default-project fallback instead would leave the registry the prompt
	// binds to unconfigured, and its first turn would come back
	// finishReason "no_provider" despite the client's own selection.
	ProjectPath string `json:"projectPath,omitempty"`
}

type ProviderSetData struct {
	SessionID    string `json:"sessionId"`
	ProviderName string `json:"providerName"`
	Model        string `json:"model,omitempty"`
}

// SetModelOptionsData tunes the current provider's request shape for a
// session's next turn, without swapping the provider/model itself (that's
// set_provider) — reasoning effort and thinking budget are provider-specific
// and ignored by providers that don't support them (e.g. CLI-subprocess
// kinds); ContextWindow resizes the session's own context-fullness budget.
// Zero/empty fields are left unchanged.
type SetModelOptionsData struct {
	SessionID       string `json:"sessionId,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	ThinkingBudget  int    `json:"thinkingBudget,omitempty"`
	ContextWindow   int    `json:"contextWindow,omitempty"`

	// ProjectPath — see SetProviderData.ProjectPath; same reason, since a
	// brand-new chat can have its model options tuned before its first
	// prompt binds the session to a project.
	ProjectPath string `json:"projectPath,omitempty"`
}

type ModelOptionsSetData struct {
	SessionID       string `json:"sessionId"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	ThinkingBudget  int    `json:"thinkingBudget,omitempty"`
	ContextWindow   int    `json:"contextWindow,omitempty"`
}

// SetPlanModeData toggles a session between normal execution and plan mode
// on its next turn: while enabled, the model only sees read-only tool
// definitions (file_read), so it can investigate and propose an approach
// without being able to write files or run commands (see SPEC.md §4c).
type SetPlanModeData struct {
	SessionID string `json:"sessionId,omitempty"`
	Enabled   bool   `json:"enabled"`

	// ProjectPath — see SetProviderData.ProjectPath; same reason, since plan
	// mode can be toggled on a brand-new chat before its first prompt binds
	// the session to a project.
	ProjectPath string `json:"projectPath,omitempty"`
}

type PlanModeSetData struct {
	SessionID string `json:"sessionId"`
	Enabled   bool   `json:"enabled"`
}

// OAuthStartData begins an OpenRouter PKCE login; Name is the provider name
// to register once it completes.
type OAuthStartData struct {
	Name        string `json:"name"`
	CallbackURL string `json:"callbackUrl"`
}

type OAuthURLData struct {
	RequestID string `json:"requestId"`
	URL       string `json:"url"`
}

type OAuthCompleteData struct {
	RequestID string `json:"requestId"`
	Code      string `json:"code"`
}

type RemoveProviderData struct {
	Name string `json:"name"`
}

type ProviderRemovedData struct {
	Name string `json:"name"`
}

// SessionSummary describes one persisted session for a `session_list`
// response, without pulling its full message history over the wire.
type SessionSummary struct {
	SessionID       string    `json:"sessionId"`
	UpdatedAt       time.Time `json:"updatedAt"`
	MessageCount    int       `json:"messageCount"`
	Title           string    `json:"title,omitempty"`
	Archived        bool      `json:"archived,omitempty"`
	LinkedSessionID *string   `json:"linkedSessionId,omitempty"`
}

// SetSessionTitleData renames a persisted session. The server never
// generates titles itself — a client derives one (e.g. from its first
// prompt) and pushes it here, so there's one heuristic to keep in sync
// rather than two.
type SetSessionTitleData struct {
	SessionID   string `json:"sessionId"`
	Title       string `json:"title"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// SetSessionArchivedData sets or clears a session's archived flag.
type SetSessionArchivedData struct {
	SessionID   string `json:"sessionId"`
	Archived    bool   `json:"archived"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// DeleteSessionData permanently removes a persisted session, archived or
// not — unlike SetSessionArchivedData, this is not reversible.
type DeleteSessionData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// SessionDeletedData confirms a delete_session, echoing the id back since
// there's no more session state left to summarize.
type SessionDeletedData struct {
	SessionID string `json:"sessionId"`
}

// SetSessionLayoutData pairs (or unpairs, with a nil LinkedSessionID) two
// sessions for a client-side split/sidebar layout. The backend just stores
// and echoes this back — it's not used for anything on the backend side.
type SetSessionLayoutData struct {
	SessionID       string  `json:"sessionId"`
	LinkedSessionID *string `json:"linkedSessionId"`
	ProjectPath     string  `json:"projectPath,omitempty"`
}

// ListSessionsData requests the sessions persisted for one project.
// ProjectPath is required unless the server has a default project (the
// -project flag it was started with).
type ListSessionsData struct {
	ProjectPath string `json:"projectPath,omitempty"`
}

type SessionListData struct {
	ProjectPath string           `json:"projectPath"`
	Sessions    []SessionSummary `json:"sessions"`
}

// GetSessionHistoryData requests the full persisted message history for one
// session — e.g. a second device opening an existing session cold, or a
// client reconnecting after being offline and needing to repaint a
// transcript it wasn't connected to see live. ProjectPath resolves the same
// way user_prompt does (§4b): required only if the session isn't already
// bound to a project and the server has no default project.
type GetSessionHistoryData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// HistoryToolCall/HistoryMessage mirror agent.ToolCall/agent.Message for the
// wire. Duplicated here rather than imported: internal/protocol is a
// lower-level package that internal/agent itself depends on, so the
// dependency can't run the other way.
type HistoryToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type HistoryMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	Reasoning  string            `json:"reasoning,omitempty"`
	ToolCalls  []HistoryToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`

	// Author is who actually sent this message, for a shared session with
	// more than one connected peer — set only on a user message whose
	// sender had a resolved (token-backed) identity; empty otherwise.
	Author string `json:"author,omitempty"`

	// AuthorAvatar is that sender's picture, stored with the message so it
	// survives a reload — either a short emoji glyph or a data URL, whichever
	// the sending client uses (SPEC.md §2a's avatar conventions). Omitted for
	// an assistant/tool message, and for a sender who never set one.
	AuthorAvatar string `json:"authorAvatar,omitempty"`

	// Provider/Model are the registered provider name and model id that
	// generated this message (assistant only). Empty for a message
	// persisted before this field existed, or for user/tool messages —
	// treat absence as "unknown", not an error.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// CreatedAt is when this message was produced. GenerationMs is how long
	// the model round-trip that produced it took (assistant only).
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	GenerationMs int64     `json:"generationMs,omitempty"`

	// Usage is this round-trip's own token/cost accounting (assistant
	// only) — not the whole turn's. InputTokens is already the full
	// conversation resent as this round-trip's request, so it doubles as
	// "tokens in up to this point"; OutputTokens is just this round-trip's
	// own reply. Omitted for user/tool messages and for a message
	// persisted before this field existed.
	Usage *UsageData `json:"usage,omitempty"`
}

// SessionHistoryData is the response to get_session_history: the session's
// full stored message history, in order. Messages is empty (not an error)
// for a sessionId that has no persisted history yet — a client can call
// this unconditionally on open without first checking existence.
type SessionHistoryData struct {
	SessionID string           `json:"sessionId"`
	Messages  []HistoryMessage `json:"messages"`
}

// OpenProjectData validates and lazily initializes a project (its own
// sandbox boundary, permissions, snapshots, sessions) without starting any
// turn — useful for a client to confirm a folder is usable, and/or to list
// its existing sessions, before creating a new one in it.
type OpenProjectData struct {
	Path string `json:"path"`
}

type ProjectOpenedData struct {
	Path        string `json:"path"`
	SandboxTier string `json:"sandboxTier,omitempty"`
}

// ListDirectoryData requests one directory's immediate entries, for a
// project-picker that works against the server's filesystem instead of the
// client's — the server may be remote. Path omitted means the server
// process's home directory.
type ListDirectoryData struct {
	Path string `json:"path,omitempty"`
}

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

// DirectoryListData responds to list_directory: Path is the directory that
// was listed (its resolved absolute form), Entries sorted dirs-first then
// alphabetically.
type DirectoryListData struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
}

// ListSnapshotsData requests the snapshots captured for one project, so a
// client can populate a restore-snapshot picker. Resolves the same way
// list_sessions does.
type ListSnapshotsData struct {
	ProjectPath string `json:"projectPath,omitempty"`
}

type SnapshotSummary struct {
	SnapshotID  string    `json:"snapshotId"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

type SnapshotListData struct {
	ProjectPath string            `json:"projectPath"`
	Snapshots   []SnapshotSummary `json:"snapshots"`
}

// PeerInfo describes one live connection to this server process. PeerID is
// stable per-connection, not per-user — two tabs/devices from the same
// person are two peers, each independently nameable/disconnectable.
type PeerInfo struct {
	PeerID      string    `json:"peerId"`
	Name        string    `json:"name"`
	IP          string    `json:"ip,omitempty"`
	DeviceType  string    `json:"deviceType"`
	Avatar      string    `json:"avatar,omitempty"` // data URL, see AuthData.Avatar
	ConnectedAt time.Time `json:"connectedAt"`
}

// PeerListData is pushed to every connected peer whenever the connection
// roster changes (connect, disconnect, rename, avatar change) — not
// requested, so a frontend's peer list never goes stale sitting in a poll.
type PeerListData struct {
	Peers []PeerInfo `json:"peers"`
}

// SetPeerNameData renames the connection sending it — there's no target
// peerId, a peer can only rename itself. Confirmed via the next PeerListData
// push.
type SetPeerNameData struct {
	Name string `json:"name"`
}

// SetPeerAvatarData sets the connection sending it own avatar — same
// self-only shape as SetPeerNameData, for updating the picture chosen at
// connect time (AuthData.Avatar) without reconnecting, e.g. after the user
// changes it in Settings. Confirmed via the next PeerListData push.
type SetPeerAvatarData struct {
	Avatar string `json:"avatar"`
}

// DisconnectPeerData force-closes another live connection. Any authenticated
// peer may disconnect any other — see the handler for why that's the right
// default under the current single-shared-token trust model.
type DisconnectPeerData struct {
	PeerID string `json:"peerId"`
}

// SetRemoteAccessData opens or closes the optional TCP listener live,
// local-socket-only (a remote peer can't be allowed to open the door it's
// not already through).
type SetRemoteAccessData struct {
	Enabled bool `json:"enabled"`
}

type RemoteAccessSetData struct {
	Enabled bool `json:"enabled"`
}

// SetRemotePasswordData rotates the TCP listener's shared auth token,
// local-socket-only. Existing TCP connections keep their session; only new
// auth handshakes need the new password.
type SetRemotePasswordData struct {
	Password string `json:"password"`
}

type RemotePasswordSetData struct{}

// RegisterTokenData mints a new named per-person TCP auth token — local-
// socket-only, same as SetRemotePasswordData. Unlike the single shared
// token, each named token authenticates and identifies one person: once
// presented in an `auth` handshake, its Name becomes that connection's
// persisted peer identity (overriding any self-reported name/avatar in the
// auth frame), letting one person's credential be revoked (remove_token)
// without rotating everyone else's.
type RegisterTokenData struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// TokenRegisteredData carries the newly generated token — shown only once,
// at creation, matching TokenListData being scrubbed.
type TokenRegisteredData struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	Email string `json:"email,omitempty"`
}

// TokenSummary is one row of a token_list response — no raw token value,
// same precedent as ProviderList scrubbing apiKey.
type TokenSummary struct {
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	HasAvatar bool   `json:"hasAvatar"`
}

type TokenListData struct {
	Tokens []TokenSummary `json:"tokens"`
}

type RemoveTokenData struct {
	Name string `json:"name"`
}

type TokenRemovedData struct {
	Name string `json:"name"`
}

// SetServerIdentityData sets this server's display name, shown to peers.
// Persisted under -state-dir so it survives a restart.
type SetServerIdentityData struct {
	Name string `json:"name"`
}

// ServerIdentityData is pushed to a peer on connect and to every peer
// whenever the identity changes.
type ServerIdentityData struct {
	Identity string `json:"identity"`

	// ServerID is this server's stable, never-changing identifier — unlike
	// Identity, it never changes and is only ever included, never set via
	// the wire (there is no set_server_id). A client uses it to recognize
	// "the same daemon" across reconnects and across different addresses,
	// and as a stable key for any per-server state it keeps. Omitted (older
	// server) rather than absent-as-error — a client talking to a server
	// that predates this field should treat that connection as unknown/
	// legacy rather than failing.
	ServerID string `json:"serverId,omitempty"`
}

// SetAutoGenerateTitleData sets the server-wide default applied to a first
// user_prompt that omits its own autoGenerateTitle (§3.1). Persisted under
// -state-dir so it survives a restart.
type SetAutoGenerateTitleData struct {
	Enabled bool `json:"enabled"`
}

// AutoGenerateTitleData is pushed to a peer on connect and to every peer
// whenever the server-wide default changes.
type AutoGenerateTitleData struct {
	Enabled bool `json:"enabled"`
}

// ListProjectsData takes no fields; it's a request marker.
type ListProjectsData struct{}

// ProjectListData enumerates projects opened on this server process since
// it started — not a filesystem scan, and not persisted across restarts.
type ProjectListData struct {
	Paths []string `json:"paths"`
}

// RemoveProjectData drops a project from this server process's opened set
// (see ProjectRegistry.Remove) — "stop offering/tracking this one", not a
// filesystem delete. A session still bound to it keeps working; the project
// just gets re-opened fresh on its next reference.
type RemoveProjectData struct {
	Path string `json:"path"`
}

type ProjectRemovedData struct {
	Path string `json:"path"`
}

// ListFilesData requests every file path under a project, for client-side
// @file-mention autocomplete. ProjectPath is required unless the server has
// a default project (the -project flag it was started with).
type ListFilesData struct {
	ProjectPath string `json:"projectPath,omitempty"`
}

// FileListData enumerates every regular file under the project root as a
// path relative to it (forward-slash separated), skipping VCS/dependency/
// build directories and anything dot-prefixed. Response to list_files.
type FileListData struct {
	ProjectPath string   `json:"projectPath"`
	Files       []string `json:"files"`
}

// CLIAvailabilityData reports whether the `claude`/`codex` binaries are on
// PATH on the machine running the backend (not the frontend's machine).
type CLIAvailabilityData struct {
	Claude bool `json:"claude"`
	Codex  bool `json:"codex"`
}

// PingData/PongData carry an optional client-chosen nonce so a round trip
// can be correlated and timed; either side may initiate a ping.
type PingData struct {
	Nonce string `json:"nonce,omitempty"`
}

type PongData struct {
	Nonce string `json:"nonce,omitempty"`
}

// UserPromptData starts or continues a session. ProjectPath is only
// meaningful the first time a given sessionId is used — it binds that
// session to a project (opening it if necessary). Once a session exists,
// its project is fixed; sending a different ProjectPath for the same
// sessionId later is a conflict, not a switch (see §6c).
type UserPromptData struct {
	Prompt            string `json:"prompt"`
	SessionID         string `json:"sessionId,omitempty"`
	ProjectPath       string `json:"projectPath,omitempty"`
	AutoGenerateTitle *bool  `json:"autoGenerateTitle,omitempty"`

	// AuthorName/AuthorAvatar are the sender's own display name and picture,
	// persisted with the stored user message so a transcript repainted from
	// session_history still shows who wrote it (an avatar is not otherwise
	// recoverable: it exists only in the sending client's settings).
	//
	// A token-authenticated TCP sender's name wins over AuthorName (SPEC.md
	// §2a: a real credential isn't overridden by a self-report). For the unix
	// socket — the local case, where file permissions are the boundary and
	// there's no credential to attribute to — this is the only source, which is
	// why it's accepted rather than ignored as it was when only token-backed
	// senders could be named.
	AuthorName   string `json:"authorName,omitempty"`
	AuthorAvatar string `json:"authorAvatar,omitempty"`
}

type CancelTurnData struct {
	SessionID string `json:"sessionId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
}

type ModelDeltaData struct {
	Content string `json:"content,omitempty"`

	// Reasoning is a chunk of the model's own reasoning/chain-of-thought
	// text (Anthropic extended thinking, or an OpenAI-compatible provider's
	// reasoning_content — DeepSeek-reasoner and similar), mutually
	// exclusive with Content on any one event. A client that doesn't know
	// this field can safely ignore it; one that does can render it as its
	// own collapsible section instead of mixing it into the answer.
	Reasoning string `json:"reasoning,omitempty"`
}

type ToolCallData struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type PermissionRequestData struct {
	ToolName    string         `json:"toolName"`
	Description string         `json:"description"`
	RequiresAsk bool           `json:"requiresAsk"`
	CacheKey    string         `json:"cacheKey"`
	Diff        *diff.FileDiff `json:"diff,omitempty"` // For rich UI diff previews
}

type PermissionResponseData struct {
	Allowed  bool   `json:"allowed"`
	Remember bool   `json:"remember"`
	Feedback string `json:"feedback,omitempty"`
}

type ToolResultData struct {
	ToolName string `json:"toolName"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

type UsageData struct {
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	TotalTokens  int     `json:"totalTokens"`
	CostUSD      float64 `json:"costUsd"`

	// CostKnown marks CostUSD as an authoritative cost reported by the
	// provider itself (e.g. a CLI subprocess's own billing total), as
	// opposed to zero-value default. Not part of the wire contract — it only
	// controls whether the agent falls back to the static per-model pricing
	// table, since a bare CostUSD==0 can't otherwise distinguish "provider
	// hasn't reported a cost" from "provider reported a genuine $0".
	CostKnown bool `json:"-"`
}

// ContextData reports how full the model's context window is after a turn,
// so a frontend can render a context meter.
type ContextData struct {
	EstimatedTokens int `json:"estimatedTokens"`
	MaxTokens       int `json:"maxTokens"`
}

// SpendData reports running cost totals, persisted across process restarts.
type SpendData struct {
	SessionTotalUSD float64 `json:"sessionTotalUsd"`
	ProjectTotalUSD float64 `json:"projectTotalUsd"`
}

type TurnCompletedData struct {
	FinishReason string       `json:"finishReason"`
	SessionID    string       `json:"sessionId"`
	Usage        *UsageData   `json:"usage,omitempty"`
	Context      *ContextData `json:"context,omitempty"`
	Spend        *SpendData   `json:"spend,omitempty"`
	DurationMs   int64        `json:"durationMs"`
}

// SnapshotCapturingData announces that a pre-mutation snapshot capture is
// starting for a turn, before the (normally invisible) file-copy work
// happens — sent right before the SnapshotStore call, so a frontend can show
// a "snapshotting..." indicator on a large file instead of a silent gap
// between tool_call_requested and the eventual tool_result.
type SnapshotCapturingData struct {
	TurnID      string `json:"turnId"`
	Description string `json:"description"`
}

// SnapshotEventData confirms a turn's snapshot was created or extended to
// cover an additional file. Files is the snapshot's full current file list
// (every path captured for this turn so far, not just the one just added).
type SnapshotEventData struct {
	SnapshotID  string   `json:"snapshotId"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

// RestoreSnapshotData resolves its project the same way user_prompt does:
// ProjectPath if given, else whatever project the envelope's sessionId is
// already bound to.
type RestoreSnapshotData struct {
	SnapshotID  string `json:"snapshotId"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// SessionTakenOverData notifies a previously connected client that another
// connection has taken over its session (last-writer-wins reconnect).
type SessionTakenOverData struct {
	SessionID string `json:"sessionId"`
}

type ErrorData struct {
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
	// ProjectPath echoes back the project a list_sessions/list_files request
	// named, so a client correlating responses by project (there's no other
	// request id) can match a failure to its request instead of only ever
	// seeing errors on success. Empty for every other error.
	ProjectPath string `json:"projectPath,omitempty"`
}

type IPCStream struct {
	reader *bufio.Scanner
	writer io.Writer
	mu     sync.Mutex
}

// splitLineStrictLF is bufio.ScanLines with the CRLF tolerance removed:
// SPEC.md §2 requires "Carriage returns (\r) must not precede the newline",
// but ScanLines silently strips a trailing \r before handing back the line,
// which accepts CRLF frames instead of rejecting them. This splits on '\n'
// the same way, except a '\r' immediately before it is a malformed frame
// (returned as an error) rather than silently stripped or accepted.
func splitLineStrictLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		line := data[:i]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			return 0, nil, fmt.Errorf("malformed frame: carriage return before newline")
		}
		return i + 1, line, nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func NewIPCStream(r io.Reader, w io.Writer) *IPCStream {
	scanner := bufio.NewScanner(r)
	scanner.Split(splitLineStrictLF)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, MaxFrameSize)
	return &IPCStream{
		reader: scanner,
		writer: w,
	}
}

func (s *IPCStream) ReadEnvelope() (*Envelope, error) {
	if !s.reader.Scan() {
		if err := s.reader.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	line := s.reader.Bytes()
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("malformed json line: %w", err)
	}
	return &env, nil
}

func (s *IPCStream) Send(env *Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal envelope: %w", err)
	}
	raw = append(raw, '\n')
	_, err = s.writer.Write(raw)
	return err
}

func (s *IPCStream) SendEvent(evtType EventType, sessionID, turnID, toolCallID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	return s.Send(&Envelope{
		Type:       evtType,
		SessionID:  sessionID,
		TurnID:     turnID,
		ToolCallID: toolCallID,
		Data:       raw,
	})
}
