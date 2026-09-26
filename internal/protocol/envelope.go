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
	EventEditMessage          EventType = "edit_message"
	EventRetryMessage         EventType = "retry_message"
	EventSwitchBranch         EventType = "switch_branch"
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
	EventSetMode              EventType = "set_mode"
	EventSetWebToolsEnabled   EventType = "set_web_tools_enabled"
	EventAskUserAnswer        EventType = "ask_user_answer"
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
	EventListNotifications    EventType = "list_notifications"
	EventDismissNotification  EventType = "dismiss_notification"
	EventUploadBegin          EventType = "upload_begin"
	EventUploadChunk          EventType = "upload_chunk"

	// Terminal events (§ built-in terminal): a real, interactive shell —
	// distinct from the bash tool's single-command-no-shell design — that a
	// human peer drives directly. Terminals are scoped per project (like
	// list_files/list_sessions), not per chat session: a project's terminal
	// tabs are the same set regardless of which chat is open, so more than
	// one connected peer/tab/device sees the same tabs. TerminalID is always
	// client-chosen (never server-allocated, same convention t3.chat's own
	// terminal panel uses) so a client can address "the tab it just opened"
	// without a request/response round trip.
	EventTerminalOpen   EventType = "terminal_open"
	EventTerminalAttach EventType = "terminal_attach"
	EventTerminalInput  EventType = "terminal_input"
	EventTerminalResize EventType = "terminal_resize"
	EventTerminalClose  EventType = "terminal_close"
	EventListTerminals  EventType = "list_terminals"

	EventModelDelta               EventType = "model_delta"
	EventToolCallRequested        EventType = "tool_call_requested"
	EventPermissionRequest        EventType = "permission_request"
	EventToolResult               EventType = "tool_result"
	EventTurnStarted              EventType = "turn_started"
	EventTurnCompleted            EventType = "turn_completed"
	EventSnapshotCapturing        EventType = "snapshot_capturing"
	EventSnapshotCreated          EventType = "snapshot_created"
	EventSnapshotRestored         EventType = "snapshot_restored"
	EventError                    EventType = "error"
	EventProviderRegistered       EventType = "provider_registered"
	EventProviderList             EventType = "provider_list"
	EventProviderSet              EventType = "provider_set"
	EventModelOptionsSet          EventType = "model_options_set"
	EventOAuthURL                 EventType = "openrouter_oauth_url"
	EventProviderRemoved          EventType = "provider_removed"
	EventModelList                EventType = "model_list"
	EventFavoriteModelsSet        EventType = "favorite_models_set"
	EventSessionList              EventType = "session_list"
	EventSessionUpdated           EventType = "session_updated"
	EventSessionDeleted           EventType = "session_deleted"
	EventDirectoryList            EventType = "directory_list"
	EventSnapshotList             EventType = "snapshot_list"
	EventCLIAvailability          EventType = "cli_availability"
	EventPong                     EventType = "pong"
	EventProjectOpened            EventType = "project_opened"
	EventProjectList              EventType = "project_list"
	EventProjectRemoved           EventType = "project_removed"
	EventFileList                 EventType = "file_list"
	EventModeSet                  EventType = "mode_set"
	EventWebToolsEnabledSet       EventType = "web_tools_enabled_set"
	EventAskUserQuestionRequested EventType = "ask_user_question_requested"
	EventPlanWritten              EventType = "plan_written"
	EventSessionHistory           EventType = "session_history"
	EventPeerList                 EventType = "peer_list"
	EventRemoteAccessSet          EventType = "remote_access_set"
	EventRemotePasswordSet        EventType = "remote_password_set"
	EventServerIdentity           EventType = "server_identity"
	EventAutoGenerateTitle        EventType = "auto_generate_title"
	EventTokenRegistered          EventType = "token_registered"
	EventTokenList                EventType = "token_list"
	EventTokenRemoved             EventType = "token_removed"
	EventNotification             EventType = "notification"
	EventGitStatus                EventType = "git_status"
	EventPendingAsks              EventType = "pending_asks"
	EventNotificationList         EventType = "notification_list"
	EventNotificationDismissed    EventType = "notification_dismissed"
	EventUploadProgress           EventType = "upload_progress"
	EventUploadDone               EventType = "upload_done"

	// EventTerminalSnapshot answers terminal_open/terminal_attach: the
	// terminal's current state plus its buffered scrollback, so a client
	// (a newly opened tab, or a second device/tab attaching to one already
	// running) repaints correctly before any live terminal_output arrives —
	// the same "state, not just a future stream" shape get_session_history
	// gives a reconnecting chat client.
	EventTerminalSnapshot EventType = "terminal_snapshot"
	// EventTerminalOutput carries one chunk of raw PTY output. Sequence is
	// monotonic per terminal (from the same counter the snapshot's
	// ReplayedThrough reports), so a client can detect a gap (a dropped
	// connection that missed events) and re-attach for a fresh snapshot
	// instead of silently rendering out-of-order bytes.
	EventTerminalOutput EventType = "terminal_output"
	// EventTerminalExited announces the shell process itself exiting (the
	// user typed `exit`, or it crashed) — the terminal (tab) still exists
	// and keeps its scrollback until an explicit terminal_close.
	EventTerminalExited EventType = "terminal_exited"
	// EventTerminalClosed confirms terminal_close: the terminal is gone,
	// scrollback and all — a client should drop the tab.
	EventTerminalClosed EventType = "terminal_closed"
	EventTerminalError  EventType = "terminal_error"
	EventTerminalList   EventType = "terminal_list"
	// EventTerminalIndex is the server-wide terminal roster, pushed to every
	// peer on connect and whenever a terminal starts, exits or closes.
	EventTerminalIndex EventType = "terminal_index"

	// Project files and diffs (§3.1 "Project files"). Each request's
	// requestId is echoed on its reply.
	EventListTree     EventType = "list_tree"
	EventTreeList     EventType = "tree_list"
	EventReadFile     EventType = "read_file"
	EventFileContent  EventType = "file_content"
	EventWriteFile    EventType = "write_file"
	EventFileWritten  EventType = "file_written"
	EventFsOp         EventType = "fs_op"
	EventFsOpDone     EventType = "fs_op_done"
	EventFilesChanged EventType = "files_changed"
	EventListChanges  EventType = "list_git_changes"
	EventGitChanges   EventType = "git_changes"
	EventGetFileDiff  EventType = "get_file_diff"
	EventFileDiff     EventType = "file_diff"
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
	// DeviceID is a stable id the client generates once and keeps, so the
	// same device is recognizable across reconnects and across servers.
	DeviceID string `json:"deviceId,omitempty"`
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

// SetModeData switches a session between the four tiers on its next turn —
// "read_only", "read_write", "autonomous", or "plan" (see SPEC.md §4c).
type SetModeData struct {
	SessionID string `json:"sessionId,omitempty"`
	Mode      string `json:"mode"`

	// ProjectPath — see SetProviderData.ProjectPath; same reason, since mode
	// can be toggled on a brand-new chat before its first prompt binds the
	// session to a project.
	ProjectPath string `json:"projectPath,omitempty"`
}

type ModeSetData struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
}

// SetWebToolsEnabledData toggles whether web_search/web_fetch are exposed to
// the model for an entire project (all sessions), independent of mode.
// Persisted, so unlike mode it survives a restart.
type SetWebToolsEnabledData struct {
	ProjectPath string `json:"projectPath,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type WebToolsEnabledSetData struct {
	ProjectPath string `json:"projectPath,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// AskUserAnswerData answers an outstanding ask_user_question_requested
// event — correlated by the envelope's own toolCallId field, exactly like
// PermissionResponseData.
type AskUserAnswerData struct {
	Answer string `json:"answer"`
}

// AskUserQuestionRequestedData is sent when the model calls ask_user_question
// — pauses the turn until an ask_user_answer (correlated by toolCallId)
// resolves it (see Agent.HandleAskUserAnswer).
type AskUserQuestionRequestedData struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// PlanWrittenData carries the plan, so clients never read the server's disk.
// Path is where the server stored it.
type PlanWrittenData struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
	// Mode is the session's persisted current mode ("read_only"/"read_write"/
	// "autonomous"/"plan"), empty for the default (read_write) — see §4c.
	Mode string `json:"mode,omitempty"`
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
	NativeToolInfo
}

type HistoryMessage struct {
	// ID addresses this message for edit_message/retry_message/
	// switch_branch. Empty only for a message persisted before this field
	// existed and never since migrated (see agent.LoadSession) — treat
	// absence as "not addressable" rather than an error.
	ID         string            `json:"id,omitempty"`
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	Reasoning  string            `json:"reasoning,omitempty"`
	ToolCalls  []HistoryToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	// Diffs: a native tool result's file changes (see ToolResultData.Diffs).
	Diffs []*diff.FileDiff `json:"diffs,omitempty"`

	// AlternateIDs lists the OTHER versions that exist at this point in the
	// conversation — other edits of this same user message, or other
	// retried responses to the same prompt — each individually addressable
	// via switch_branch. Omitted when this message has no alternates (the
	// common case, and always true for a message that was never edited or
	// retried). This message's own ID is never included in its own list.
	AlternateIDs []string `json:"alternateIds,omitempty"`

	// Author is who actually sent this message, for a shared session with
	// more than one connected peer — set only on a user message whose
	// sender had a resolved (token-backed) identity; empty otherwise.
	Author string `json:"author,omitempty"`

	// AuthorAvatar is that sender's picture, stored with the message so it
	// survives a reload — either a short emoji glyph or a data URL, whichever
	// the sending client uses (SPEC.md §2a's avatar conventions). Omitted for
	// an assistant/tool message, and for a sender who never set one.
	AuthorAvatar string `json:"authorAvatar,omitempty"`

	// Attachments are the server-side paths this user message attached
	// (user_prompt's own attachments), so a repainted transcript can show them.
	Attachments []string `json:"attachments,omitempty"`

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
	// Mode is this session's persisted current mode — see SessionSummary.Mode.
	// This is the authoritative value a client should adopt on session open,
	// rather than replaying its own possibly-stale cached mode (§4b/§4c):
	// another peer may have changed it since this client last had the
	// session open.
	Mode string `json:"mode,omitempty"`
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

	// Error is set (with Path echoing the request, Entries empty) when the
	// directory couldn't be listed, so a folder browser can say why rather
	// than wait on a reply that never comes.
	Error string `json:"error,omitempty"`
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
	// Files are the project-relative paths this snapshot holds a pre-turn
	// copy (or recorded absence) of, forward-slash separated.
	Files []string `json:"files"`
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
	DeviceID    string    `json:"deviceId,omitempty"` // AuthData.DeviceID
	// The chat this connection last opened (get_session_history), if any.
	SessionID   string `json:"sessionId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// PeerListData is pushed to every connected peer whenever the connection
// roster changes (connect, disconnect, rename, avatar change) — not
// requested, so a frontend's peer list never goes stale sitting in a poll.
type PeerListData struct {
	Peers []PeerInfo `json:"peers"`
	// You is the receiving connection's own PeerID, so a client can tell
	// itself apart from everyone else.
	You string `json:"you,omitempty"`
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
// at creation, matching TokenListData being scrubbed. CaPem is the server's
// auto-managed CA certificate (empty if it's running an operator-supplied
// one instead, see remoteControl.CAPEM) — bundled alongside the token so a
// client can be paired in one copy/paste instead of two: the token alone is
// only as trustworthy as the TLS connection it's sent over, and this CA is
// what lets a client verify that connection actually terminates at this
// server rather than an operator-supplied cert whose issuer the client's own
// OS trust store already covers.
type TokenRegisteredData struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	Email string `json:"email,omitempty"`
	CaPem string `json:"caPem,omitempty"`
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

// ProjectListData enumerates every project this server has ever opened, most-
// recently-opened first — not a filesystem scan, backed by projects.json so
// it survives a restart (see projectstore.Store).
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

// FileRequestData is list_tree/read_file/get_file_diff: Path is relative to
// the project root, forward-slash separated ("" is the root itself).
type FileRequestData struct {
	RequestID   string `json:"requestId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	Path        string `json:"path"`
	// get_file_diff only: diff against this snapshot's copy instead of
	// HEAD, or against HEAD's copy of BasePath (a rename's old path).
	SnapshotID string `json:"snapshotId,omitempty"`
	BasePath   string `json:"basePath,omitempty"`
}

type TreeEntry struct {
	Name      string `json:"name"`
	IsDir     bool   `json:"isDir"`
	IsSymlink bool   `json:"isSymlink,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

type TreeListData struct {
	RequestID   string      `json:"requestId,omitempty"`
	ProjectPath string      `json:"projectPath"`
	Path        string      `json:"path"`
	Entries     []TreeEntry `json:"entries"`
	Error       string      `json:"error,omitempty"`
}

// FileContentData answers read_file. Version identifies this content for
// write_file's conflict check. Content is empty when Binary or TooLarge.
type FileContentData struct {
	RequestID   string `json:"requestId,omitempty"`
	ProjectPath string `json:"projectPath"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	Version     string `json:"version,omitempty"`
	Size        int64  `json:"size"`
	Binary      bool   `json:"binary,omitempty"`
	TooLarge    bool   `json:"tooLarge,omitempty"`
	Error       string `json:"error,omitempty"`
}

// WriteFileData saves a file. BaseVersion is the read_file Version the edit
// started from ("" for a new file); a mismatch is a conflict, not a write.
type WriteFileData struct {
	RequestID   string `json:"requestId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	BaseVersion string `json:"baseVersion"`
	Force       bool   `json:"force,omitempty"`
}

type FileWrittenData struct {
	RequestID   string `json:"requestId,omitempty"`
	ProjectPath string `json:"projectPath"`
	Path        string `json:"path"`
	Version     string `json:"version,omitempty"`
	Conflict    bool   `json:"conflict,omitempty"`
	Error       string `json:"error,omitempty"`
}

// FsOpData is one explorer action. Op is "create" (empty file), "mkdir",
// "rename" (to NewPath) or "delete" (recursive for a folder).
type FsOpData struct {
	RequestID   string `json:"requestId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	Op          string `json:"op"`
	Path        string `json:"path"`
	NewPath     string `json:"newPath,omitempty"`
}

type FsOpDoneData struct {
	RequestID string `json:"requestId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// FilesChangedData is pushed to every peer after write_file/fs_op, naming
// the paths touched, so open explorers and editors can refresh.
type FilesChangedData struct {
	ProjectPath string   `json:"projectPath"`
	Paths       []string `json:"paths"`
}

// GitChange is one path from `git status`. Index/Worktree are porcelain's
// X/Y letters ("." unchanged, "?" untracked); Conflict marks an unmerged path.
type GitChange struct {
	Path     string `json:"path"`
	OrigPath string `json:"origPath,omitempty"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
	Conflict bool   `json:"conflict,omitempty"`
}

type GitChangesData struct {
	RequestID   string      `json:"requestId,omitempty"`
	ProjectPath string      `json:"projectPath"`
	Files       []GitChange `json:"files"`
	Truncated   bool        `json:"truncated,omitempty"`
	Error       string      `json:"error,omitempty"`
}

type FileDiffData struct {
	RequestID   string         `json:"requestId,omitempty"`
	ProjectPath string         `json:"projectPath"`
	Path        string         `json:"path"`
	Diff        *diff.FileDiff `json:"diff,omitempty"`
	Binary      bool           `json:"binary,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// CLIAvailabilityData reports whether the `claude`/`codex` binaries are on
// PATH on the machine running the backend (not the frontend's machine).
type CLIAvailabilityData struct {
	Claude bool `json:"claude"`
	Codex  bool `json:"codex"`
	// Agents covers every supported CLI (claude and codex included), keyed
	// by the provider kind that registers it ("cli_cursor", ...).
	Agents map[string]bool `json:"agents"`
}

// PingData/PongData carry an optional client-chosen nonce so a round trip
// can be correlated and timed; either side may initiate a ping.
type PingData struct {
	Nonce string `json:"nonce,omitempty"`

	// Status rides along on every server-sent ping (SPEC.md §2a) so a
	// client's status bar stays current without a separate poll. Omitted on
	// a client-sent ping and on the pong echo.
	Status *ServerStatus `json:"status,omitempty"`
}

// ServerStatus is a cheap snapshot of the server's own health. Load and
// memory fields are zero on platforms without /proc (see readHostStats).
type ServerStatus struct {
	UptimeSec     int64   `json:"uptimeSec"`
	ActiveTurns   int     `json:"activeTurns"`
	Peers         int     `json:"peers"`
	OpenProjects  int     `json:"openProjects"`
	Load1         float64 `json:"load1,omitempty"`
	CPUs          int     `json:"cpus"`
	MemTotalBytes uint64  `json:"memTotalBytes,omitempty"`
	MemAvailBytes uint64  `json:"memAvailBytes,omitempty"`
	HeapBytes     uint64  `json:"heapBytes"`
	Version       string  `json:"version,omitempty"`

	// QueuedTurns counts turns accepted but waiting to start: a session runs
	// one turn at a time, so a prompt sent while its session is busy waits.
	QueuedTurns int `json:"queuedTurns"`
	// RateLimits holds the latest subscription usage each cli_claude/
	// cli_codex provider reported, keyed by provider name.
	RateLimits map[string]ProviderRateLimits `json:"rateLimits,omitempty"`
	Spend      *ServerSpend                  `json:"spend,omitempty"`
	Budgets    []Budget                      `json:"budgets,omitempty"`
}

// RateLimitWindow is one subscription usage window. ResetsAt is RFC 3339.
type RateLimitWindow struct {
	UsedPct  float64 `json:"usedPct"`
	ResetsAt string  `json:"resetsAt,omitempty"`
}

// OtherRateLimitWindow is a window that is neither the plan's own 5-hour
// nor 7-day one: a different length (Codex's 30-day window on a free plan),
// or a narrower bucket such as a single model's weekly cap, named by Label.
type OtherRateLimitWindow struct {
	WindowMins int `json:"windowMins"`
	// Label names what a scoped window covers ("Opus", a Codex limitName).
	// Empty for a plan-wide window.
	Label string `json:"label,omitempty"`
	RateLimitWindow
}

type ProviderRateLimits struct {
	// Kind is the provider's kind ("cli_claude" / "cli_codex"), so a client
	// can show which agent the numbers belong to without a provider lookup.
	Kind string `json:"kind,omitempty"`
	// Plan is the subscription tier the CLI reports ("pro", "max", "plus",
	// "free", ...), when it does.
	Plan string `json:"plan,omitempty"`
	// UpdatedAt is when the server last heard these numbers (RFC 3339).
	UpdatedAt string                 `json:"updatedAt,omitempty"`
	FiveHour  *RateLimitWindow       `json:"fiveHour,omitempty"`
	SevenDay  *RateLimitWindow       `json:"sevenDay,omitempty"`
	Other     []OtherRateLimitWindow `json:"other,omitempty"`
}

// Windows lists every window, the 5h/7d ones included, in one shape.
func (rl ProviderRateLimits) Windows() []OtherRateLimitWindow {
	var out []OtherRateLimitWindow
	if rl.FiveHour != nil {
		out = append(out, OtherRateLimitWindow{WindowMins: 5 * 60, RateLimitWindow: *rl.FiveHour})
	}
	if rl.SevenDay != nil {
		out = append(out, OtherRateLimitWindow{WindowMins: 7 * 24 * 60, RateLimitWindow: *rl.SevenDay})
	}
	return append(out, rl.Other...)
}

// Merge overlays a partial update (one window from a streamed
// rate_limit_event) onto rl: windows it carries replace the matching ones,
// everything else is kept.
func (rl ProviderRateLimits) Merge(u ProviderRateLimits) ProviderRateLimits {
	if u.Kind != "" {
		rl.Kind = u.Kind
	}
	if u.Plan != "" {
		rl.Plan = u.Plan
	}
	if u.UpdatedAt != "" {
		rl.UpdatedAt = u.UpdatedAt
	}
	if u.FiveHour != nil {
		rl.FiveHour = u.FiveHour
	}
	if u.SevenDay != nil {
		rl.SevenDay = u.SevenDay
	}
	other := append([]OtherRateLimitWindow(nil), rl.Other...)
	for _, w := range u.Other {
		replaced := false
		for i := range other {
			if other[i].Label == w.Label && other[i].WindowMins == w.WindowMins {
				other[i], replaced = w, true
			}
		}
		if !replaced {
			other = append(other, w)
		}
	}
	rl.Other = other
	return rl
}

// ServerSpend totals recorded turn cost across every project, bucketed by
// the server's local day/calendar month.
type ServerSpend struct {
	TodayUSD float64 `json:"todayUsd"`
	MonthUSD float64 `json:"monthUsd"`
}

// Budget is one spend limit the server enforces. UsedUSD is omitted for a
// "session" scope limit, which applies to each session separately (its
// per-session usage is in every turn_completed's spend).
type Budget struct {
	Scope       string   `json:"scope"` // "server" | "project" | "session"
	ProjectPath string   `json:"projectPath,omitempty"`
	Period      string   `json:"period"` // "day" | "month" | "total"
	LimitUSD    float64  `json:"limitUsd"`
	UsedUSD     *float64 `json:"usedUsd,omitempty"`
}

// GitStatusData is one project's git state (SPEC.md §3.2 git_status).
type GitStatusData struct {
	ProjectPath string  `json:"projectPath"`
	Branch      *string `json:"branch"`
	Detached    bool    `json:"detached"`
	DirtyFiles  int     `json:"dirtyFiles"`
	Ahead       int     `json:"ahead"`
	Behind      int     `json:"behind"`
	HasUpstream bool    `json:"hasUpstream"`
	// Files lists the first MaxGitFiles changed paths; DirtyFiles is still
	// the full count.
	Files []GitFile `json:"files,omitempty"`
}

// MaxGitFiles caps GitStatusData.Files — it rides every git_status push.
const MaxGitFiles = 50

// GitFile is one changed path. Status is one letter: M modified, A added,
// D deleted, R renamed, C copied, T type change, U unmerged, ? untracked.
type GitFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// PendingAsk is one unanswered permission_request or ask_user_question.
// Exactly one of the embedded payloads is set, matching Kind, and its fields
// are flattened into the object — the same shape the original event carried.
type PendingAsk struct {
	Kind        string    `json:"kind"` // "permission" | "question"
	SessionID   string    `json:"sessionId"`
	ProjectPath string    `json:"projectPath,omitempty"`
	TurnID      string    `json:"turnId"`
	ToolCallID  string    `json:"toolCallId"`
	CreatedAt   time.Time `json:"createdAt"`
	*PermissionRequestData
	*AskUserQuestionRequestedData
}

type PendingAsksData struct {
	Asks []PendingAsk `json:"asks"`
}

// Notification is one entry in the server's shared notification feed
// (SPEC.md §3.2 notification) — broadcast to every peer when raised, and
// replayed by list_notifications for a client that connected afterward.
type Notification struct {
	ID          string    `json:"id"`
	Level       string    `json:"level"` // "info" | "success" | "warning" | "error"
	Kind        string    `json:"kind"`  // e.g. "turn_completed", "permission_request", "peer_joined"
	Title       string    `json:"title"`
	Body        string    `json:"body,omitempty"`
	SessionID   string    `json:"sessionId,omitempty"`
	ProjectPath string    `json:"projectPath,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

type NotificationListData struct {
	Notifications []Notification `json:"notifications"`
}

// DismissNotificationData dismisses one notification by ID, or every
// notification when ID is empty. Echoed back verbatim as
// notification_dismissed to every peer.
type DismissNotificationData struct {
	ID string `json:"id,omitempty"`
}

// UploadBeginData starts a chunked client→server file upload (SPEC.md §3.1
// upload_begin). SHA256 (lowercase hex) is the content address: if the
// server already holds that content, it answers upload_done immediately and
// no bytes are sent at all.
type UploadBeginData struct {
	UploadID string `json:"uploadId"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// UploadChunkData carries the next sequential slice of an upload's bytes.
type UploadChunkData struct {
	UploadID string `json:"uploadId"`
	Data     []byte `json:"data"` // base64 on the wire (encoding/json's []byte default)
}

type UploadProgressData struct {
	UploadID string `json:"uploadId"`
	Received int64  `json:"received"`
}

// UploadDoneData ends an upload: Path (an absolute path on the server,
// usable as a user_prompt attachment) on success, Error otherwise.
type UploadDoneData struct {
	UploadID string `json:"uploadId"`
	Path     string `json:"path,omitempty"`
	Error    string `json:"error,omitempty"`
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

	// Attachments are absolute paths on the server's machine the model may
	// read for the rest of this session, even outside the project root
	// (SPEC.md §3.1). Over TCP each must resolve inside the server's upload
	// store or the project; the unix socket's own user may attach anything.
	Attachments []string `json:"attachments,omitempty"`
}

// EditMessageData replaces a past user message's content and regenerates
// everything after it, as a fresh turn — same streaming events
// (model_delta/tool_call_requested/permission_request/tool_result/
// turn_completed) and cancel_turn semantics as user_prompt. Unlike
// user_prompt, SessionID is required: this acts on a specific, already-
// existing session, the same resolution rule as set_session_title (§3.1).
// MessageID must name a user-role message currently in that session's active
// history — its old content and everything generated after it are archived
// as a recoverable branch (see switch_branch), never deleted.
type EditMessageData struct {
	SessionID         string `json:"sessionId"`
	MessageID         string `json:"messageId"`
	Content           string `json:"content"`
	AutoGenerateTitle *bool  `json:"autoGenerateTitle,omitempty"`
}

// RetryMessageData discards a past assistant message and regenerates a
// fresh response from the same point, as a fresh turn — same streaming
// events and cancel_turn semantics as user_prompt/edit_message. MessageID
// must name an assistant-role message currently in the session's active
// history; it need not be the last message — retrying an earlier round-trip
// inside a multi-tool-call turn discards that round-trip and everything
// after it too. Same archive-not-delete guarantee as edit_message.
type RetryMessageData struct {
	SessionID string `json:"sessionId"`
	MessageID string `json:"messageId"`
}

// SwitchBranchData makes the branch containing MessageID the active one for
// SessionID, swapping out whatever was active at that branch point (which
// becomes recoverable in its place). Pure navigation: no model call, no
// cost. Response: session_history, same as get_session_history.
type SwitchBranchData struct {
	SessionID string `json:"sessionId"`
	MessageID string `json:"messageId"`
}

type CancelTurnData struct {
	SessionID string `json:"sessionId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
	// QueuedOnly withdraws the turn only if it hasn't started yet (§3.1).
	QueuedOnly bool `json:"queuedOnly,omitempty"`
}

// TurnStartedData is empty: the envelope's turnId is the whole message.
type TurnStartedData struct{}

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
	NativeToolInfo
}

// NativeToolInfo describes a tool call a CLI agent ran with its own tools
// (SPEC.md §6b) rather than one of yoakeland's: Name/Args are the agent's
// own tool name and input, which a client can't be expected to know every
// variant of, so Title/Kind/Locations give it something generic to render.
type NativeToolInfo struct {
	Native bool   `json:"native,omitempty"`
	Title  string `json:"title,omitempty"`
	// Kind is one of ACP's tool kinds: read | edit | delete | move | search |
	// execute | think | fetch | other.
	Kind      string   `json:"kind,omitempty"`
	Locations []string `json:"locations,omitempty"`
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
	// Diffs are the file changes a native tool call made, when the agent
	// reported them (SPEC.md §6b).
	Diffs []*diff.FileDiff `json:"diffs,omitempty"`
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
	FinishReason string `json:"finishReason"`
	SessionID    string `json:"sessionId"`
	// UserMessageID is the id of the prompt this turn is anchored to: the
	// message just created by user_prompt/edit_message, or — for
	// retry_message, which creates no new user message — the existing user
	// message the retried reply followed. Empty when the turn ended before
	// any message existed to anchor to (no_provider/budget_exceeded on a
	// session's very first message would be the only case).
	UserMessageID string `json:"userMessageId,omitempty"`
	// MessageID is the id of the last message this turn appended — normally
	// the final assistant reply. Empty if the turn ended before appending
	// one (no_provider/budget_exceeded, or a cancel before any content
	// streamed). This — not a follow-up get_session_history — is how a
	// client learns the id to target with a later edit_message/
	// retry_message/switch_branch for the message it just watched stream in.
	MessageID  string       `json:"messageId,omitempty"`
	Usage      *UsageData   `json:"usage,omitempty"`
	Context    *ContextData `json:"context,omitempty"`
	Spend      *SpendData   `json:"spend,omitempty"`
	DurationMs int64        `json:"durationMs"`
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

type ErrorData struct {
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
	// ProjectPath echoes back the project a list_sessions/list_files request
	// named, so a client correlating responses by project (there's no other
	// request id) can match a failure to its request instead of only ever
	// seeing errors on success. Empty for every other error.
	ProjectPath string `json:"projectPath,omitempty"`
}

// TerminalOpenData starts a new terminal (tab) bound to a chat session —
// terminals live and die with the chat that opened them (see CloseSession,
// invoked by delete_session): closing the chat kills every terminal it
// owns, and a session can hold several at once (its open tabs). SessionID
// resolves exactly like UserPromptData's does — ProjectPath only matters the
// first time this SessionID is used, binding it the same way a prompt
// would; omit it on a session that's already bound (e.g. one that already
// has messages). Cwd, if given, is relative to the project root (validated
// the same way a tool's file path is — escaping the root is rejected) and
// defaults to the root itself. Cols/Rows default to a conventional 80x24
// when omitted. Env adds to, never replaces, the sandbox's own fixed
// environment (see sandbox.LinuxSandbox.baseEnv) — same restricted shape
// (key pattern, count, size) as the sandboxed bash tool's environment
// already gets, so a terminal can't be used to smuggle in something the
// tool boundary wouldn't allow.
type TerminalOpenData struct {
	SessionID   string            `json:"sessionId"`
	ProjectPath string            `json:"projectPath,omitempty"`
	TerminalID  string            `json:"terminalId"`
	Cwd         string            `json:"cwd,omitempty"`
	Cols        int               `json:"cols,omitempty"`
	Rows        int               `json:"rows,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// TerminalAttachData subscribes this connection to a terminal that may
// already be running within SessionID's chat (opened by this client
// earlier, or by another tab/device watching the same chat) — the
// multi-tab/split-view case. Cols/Rows, if given, resize it immediately (the
// attaching pane's own size may differ from whoever had it open last).
// RestartIfExited respawns the shell (same Cwd/Env it was opened with) if
// the previous process already exited, instead of erroring — for "reopen
// this closed tab".
type TerminalAttachData struct {
	SessionID       string `json:"sessionId"`
	ProjectPath     string `json:"projectPath,omitempty"`
	TerminalID      string `json:"terminalId"`
	Cols            int    `json:"cols,omitempty"`
	Rows            int    `json:"rows,omitempty"`
	RestartIfExited bool   `json:"restartIfExited,omitempty"`
}

// TerminalInputData writes raw bytes to the terminal's pty, exactly as
// typed/pasted client-side — this is not a command string (no parsing,
// no argv, unlike the bash tool); the shell on the other end interprets it.
// Data is base64-encoded raw bytes, not a JSON string of the input text:
// terminal I/O is not guaranteed to be valid UTF-8 (paste of binary data,
// a multi-byte character split across two writes) and encoding/json quietly
// mangles invalid UTF-8 in a plain string (substituting U+FFFD) rather than
// erroring, which would corrupt exactly the input a user is relying on
// being sent byte-for-byte.
type TerminalInputData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
	TerminalID  string `json:"terminalId"`
	Data        string `json:"data"`
}

type TerminalResizeData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
	TerminalID  string `json:"terminalId"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

type TerminalCloseData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
	TerminalID  string `json:"terminalId"`
}

// ListTerminalsData requests every terminal (tab) open in one chat session —
// the set a client repaints that chat's tab strip from on open, resolved the
// same way GetSessionHistoryData is.
type ListTerminalsData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// TerminalStatus mirrors terminal.Status on the wire as a plain string
// rather than importing internal/terminal here (protocol is the lower-level
// package; see HistoryMessage's doc comment for the same reasoning).
type TerminalStatus string

const (
	TerminalStatusStarting TerminalStatus = "starting"
	TerminalStatusRunning  TerminalStatus = "running"
	TerminalStatusExited   TerminalStatus = "exited"
	TerminalStatusError    TerminalStatus = "error"
)

// TerminalSnapshotData answers terminal_open/terminal_attach: current state
// plus buffered scrollback (see EventTerminalSnapshot). History is
// base64-encoded raw bytes, same reasoning as TerminalInputData.Data — it's
// the same PTY output stream, just replayed instead of live. ExitCode is nil
// unless Status is "exited". Sequence is the output sequence number History
// already includes up to — a client resumes live terminal_output from
// Sequence+1 without gapping or duplicating.
type TerminalSnapshotData struct {
	TerminalID  string         `json:"terminalId"`
	Cwd         string         `json:"cwd"`
	Cols        int            `json:"cols"`
	Rows        int            `json:"rows"`
	Status      TerminalStatus `json:"status"`
	Pid         int            `json:"pid,omitempty"`
	History     string         `json:"history"`
	Sequence    uint64         `json:"sequence"`
	ExitCode    *int           `json:"exitCode,omitempty"`
	Label       string         `json:"label"`
	SandboxTier string         `json:"sandboxTier"`
}

// TerminalOutputData.Data is base64-encoded raw bytes — see
// TerminalInputData's doc comment.
type TerminalOutputData struct {
	TerminalID string `json:"terminalId"`
	Data       string `json:"data"`
	Sequence   uint64 `json:"sequence"`
}

type TerminalExitedData struct {
	TerminalID string `json:"terminalId"`
	ExitCode   int    `json:"exitCode"`
}

type TerminalClosedData struct {
	TerminalID string `json:"terminalId"`
}

type TerminalErrorData struct {
	TerminalID string `json:"terminalId"`
	Message    string `json:"message"`
}

// TerminalSummary is one row of a terminal_list response — enough for a
// client to render its tab strip without attaching to every tab up front.
type TerminalSummary struct {
	TerminalID string         `json:"terminalId"`
	Cwd        string         `json:"cwd"`
	Status     TerminalStatus `json:"status"`
	Label      string         `json:"label"`
	ExitCode   *int           `json:"exitCode,omitempty"`
	// Set only in terminal_index, where rows span sessions and projects.
	SessionID   string `json:"sessionId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// TerminalIndexData is every terminal on the server, across every chat and
// project (SPEC.md §3.2 terminal_index).
type TerminalIndexData struct {
	Terminals []TerminalSummary `json:"terminals"`
}

type TerminalListData struct {
	SessionID string            `json:"sessionId"`
	Terminals []TerminalSummary `json:"terminals"`
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
