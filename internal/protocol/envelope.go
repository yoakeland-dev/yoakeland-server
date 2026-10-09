package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/audit"
	"yoakeland/internal/diff"
)

// MaxFrameSize is the largest single newline-delimited frame the IPC
// framing (SPEC.md §2) accepts, in either direction — also reused by
// anything else that needs to cap a read against the same limit (e.g.
// tool.FileReadTool) rather than inventing a second number.
const MaxFrameSize = 10 * 1024 * 1024

type EventType string

const (
	EventUserPrompt            EventType = "user_prompt"
	EventEditMessage           EventType = "edit_message"
	EventRetryMessage          EventType = "retry_message"
	EventSwitchBranch          EventType = "switch_branch"
	EventPermissionResponse    EventType = "permission_response"
	EventCancelTurn            EventType = "cancel_turn"
	EventSteerTurn             EventType = "steer_turn"
	EventSteerAccepted         EventType = "steer_accepted"
	EventSteerRejected         EventType = "steer_rejected"
	EventRestoreSnapshot       EventType = "restore_snapshot"
	EventAuth                  EventType = "auth"
	EventRegisterProvider      EventType = "register_provider"
	EventListProviders         EventType = "list_providers"
	EventSetProvider           EventType = "set_provider"
	EventSetModelOptions       EventType = "set_model_options"
	EventSetModelRoles         EventType = "set_model_roles"
	EventOAuthStart            EventType = "openrouter_oauth_start"
	EventOAuthComplete         EventType = "openrouter_oauth_complete"
	EventRemoveProvider        EventType = "remove_provider"
	EventListModels            EventType = "list_models"
	EventSetFavoriteModels     EventType = "set_favorite_models"
	EventListSessions          EventType = "list_sessions"
	EventDetectCLI             EventType = "detect_cli"
	EventPing                  EventType = "ping"
	EventOpenProject           EventType = "open_project"
	EventListProjects          EventType = "list_projects"
	EventRemoveProject         EventType = "remove_project"
	EventListFiles             EventType = "list_files"
	EventSetMode               EventType = "set_mode"
	EventApprovePlan           EventType = "approve_plan"
	EventSetWebToolsEnabled    EventType = "set_web_tools_enabled"
	EventAskUserAnswer         EventType = "ask_user_answer"
	EventGetSessionHistory     EventType = "get_session_history"
	EventSetSessionTitle       EventType = "set_session_title"
	EventSetSessionArchived    EventType = "set_session_archived"
	EventDeleteSession         EventType = "delete_session"
	EventSetSessionLayout      EventType = "set_session_layout"
	EventListDirectory         EventType = "list_directory"
	EventListSnapshots         EventType = "list_snapshots"
	EventImportChatFiles       EventType = "import_chat_files"
	EventChatFilesImported     EventType = "chat_files_imported"
	EventSetPeerName           EventType = "set_peer_name"
	EventSetPeerAvatar         EventType = "set_peer_avatar"
	EventDisconnectPeer        EventType = "disconnect_peer"
	EventSetRemoteAccess       EventType = "set_remote_access"
	EventSetRemotePassword     EventType = "set_remote_password"
	EventSetServerIdentity     EventType = "set_server_identity"
	EventSetAutoGenerateTitle  EventType = "set_auto_generate_title"
	EventSetRuntimeSettings    EventType = "set_runtime_settings"
	EventSetDefaultProvider    EventType = "set_default_provider"
	EventRegisterToken         EventType = "register_token"
	EventListTokens            EventType = "list_tokens"
	EventRemoveToken           EventType = "remove_token"
	EventListNotifications     EventType = "list_notifications"
	EventListPermissionRules   EventType = "list_permission_rules"
	EventAddPermissionRule     EventType = "add_permission_rule"
	EventDeletePermissionRule  EventType = "delete_permission_rule"
	EventDismissNotification   EventType = "dismiss_notification"
	EventMarkNotificationsRead EventType = "mark_notifications_read"
	EventListAudit             EventType = "list_audit"
	EventSetNotifyPrefs        EventType = "set_notify_prefs"
	EventSetPresence           EventType = "set_presence"
	EventUploadBegin           EventType = "upload_begin"
	EventUploadChunk           EventType = "upload_chunk"

	// MCP servers and skills (SPEC.md §3.1 "MCP servers" and "Skills").
	EventListMCPServers      EventType = "list_mcp_servers"
	EventSaveMCPServer       EventType = "save_mcp_server"
	EventRemoveMCPServer     EventType = "remove_mcp_server"
	EventSetMCPServerEnabled EventType = "set_mcp_server_enabled"
	EventSetMCPToolEnabled   EventType = "set_mcp_tool_enabled"
	EventReconnectMCPServer  EventType = "reconnect_mcp_server"
	EventMCPOAuthStart       EventType = "mcp_oauth_start"
	EventMCPOAuthComplete    EventType = "mcp_oauth_complete"
	EventMCPSignOut          EventType = "mcp_sign_out"
	EventScanMCPImports      EventType = "scan_mcp_imports"
	EventImportMCPServers    EventType = "import_mcp_servers"
	EventListSkills          EventType = "list_skills"
	EventSetSkillEnabled     EventType = "set_skill_enabled"
	EventGetSkill            EventType = "get_skill"

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

	// Browser relay (SPEC.md § browser): the agent's browser tool drives the
	// browser panel of a desktop client that offered to host it
	// (browser_host). The server sends that one client a browser_request;
	// the client answers with a browser_response carrying the same
	// requestId.
	EventBrowserHost     EventType = "browser_host"
	EventBrowserRequest  EventType = "browser_request"
	EventBrowserResponse EventType = "browser_response"

	EventMCPServers               EventType = "mcp_servers"
	EventMCPOAuthURL              EventType = "mcp_oauth_url"
	EventMCPImportCandidates      EventType = "mcp_import_candidates"
	EventSkills                   EventType = "skills"
	EventSkillDetail              EventType = "skill_detail"
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
	EventModelRolesSet            EventType = "model_roles_set"
	EventContextCompacted         EventType = "context_compacted"
	EventGetContextBreakdown      EventType = "get_context_breakdown"
	EventContextBreakdown         EventType = "context_breakdown"
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
	EventRuntimeSettings          EventType = "runtime_settings"
	EventDefaultProviderSet       EventType = "default_provider_set"
	EventTokenRegistered          EventType = "token_registered"
	EventTokenList                EventType = "token_list"
	EventTokenRemoved             EventType = "token_removed"
	EventNotification             EventType = "notification"
	EventGitStatus                EventType = "git_status"
	EventPendingAsks              EventType = "pending_asks"
	EventNotificationList         EventType = "notification_list"
	EventPermissionRules          EventType = "permission_rules"
	EventNotificationDismissed    EventType = "notification_dismissed"
	EventNotificationsRead        EventType = "notifications_read"
	EventAuditList                EventType = "audit_list"
	EventNotifyPrefs              EventType = "notify_prefs"
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

	// Media preview (images, video, audio, PDFs and fonts the explorer can render instead of just
	// saying "binary file"): read_media answers with size/mime, then the
	// client pulls the bytes in bounded chunks with read_media_chunk. Kept
	// separate from read_file/file_content so a plain text edit never pays
	// for chunking, and a multi-hundred-MB video never has to land in one
	// frame or fully in server memory at once.
	EventReadMedia      EventType = "read_media"
	EventMediaInfo      EventType = "media_info"
	EventReadMediaChunk EventType = "read_media_chunk"
	EventMediaChunk     EventType = "media_chunk"

	// get_usage answers with usage_report: every recorded model round-trip
	// on this server, bucketed by hour (§3.1 get_usage).
	EventGetUsage    EventType = "get_usage"
	EventUsageReport EventType = "usage_report"
)

// ValidSessionID reports whether id is safe to use as a file name: session
// ids come straight off the wire and become <id>.json / <id>.md on disk, so
// a separator ("../../x") would reach any file this user can write.
func ValidSessionID(id string) bool {
	return id != "" && len(id) <= 200 && !strings.ContainsAny(id, "/\\:\x00")
}

type Envelope struct {
	Type       EventType `json:"type"`
	SessionID  string    `json:"sessionId,omitempty"`
	TurnID     string    `json:"turnId,omitempty"`
	ToolCallID string    `json:"toolCallId,omitempty"`
	// ParentToolCallID marks an event a subagent produced: the tool call
	// (a subagent call, or a CLI agent's own Task) it ran inside of. A
	// client files the event under that call instead of the main timeline.
	ParentToolCallID string          `json:"parentToolCallId,omitempty"`
	Data             json.RawMessage `json:"data,omitempty"`
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
	// FastMode turns a model's fast mode on or off; absent leaves it as is.
	FastMode *bool `json:"fastMode,omitempty"`

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
	FastMode        *bool  `json:"fastMode,omitempty"`
}

// ModelRole assigns one model role (SPEC.md §4f) to a registered API
// provider and model, with the reasoning effort that role runs at.
type ModelRole struct {
	ProviderName    string `json:"providerName"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// SetModelRolesData replaces the server-wide role table. Keys are "plan",
// "explore", "utility" and "advisor"; a role left out runs on the session's
// own model.
type SetModelRolesData struct {
	Roles map[string]ModelRole `json:"roles"`
}

// ModelRolesSetData reports the role table now in effect: the reply to
// set_model_roles, broadcast to every peer, and sent once on connect.
type ModelRolesSetData struct {
	Roles map[string]ModelRole `json:"roles"`
}

// ContextCompactedData announces a compaction the agent just recorded in a
// session's history (SPEC.md §4g). Kind is "clear" (old tool results
// replaced by stubs) or "summary" (everything before it summarized).
type ContextCompactedData struct {
	MessageID    string `json:"messageId"`
	Kind         string `json:"kind"`
	TokensBefore int    `json:"tokensBefore"`
	TokensAfter  int    `json:"tokensAfter"`
}

// GetContextBreakdownData asks for a session's context split by category
// (SPEC.md §3.1 get_context_breakdown); answered with context_breakdown.
type GetContextBreakdownData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
	RequestID   string `json:"requestId,omitempty"`
}

// Context category keys (ContextCategory.Key). A client keys its colours on
// these; Label is what to print, and a key a client doesn't know is "other".
const (
	CtxSystem      = "system"       // system prompt
	CtxTools       = "tools"        // tool definitions
	CtxMemory      = "memory"       // memory / instruction files (Claude Code)
	CtxSkills      = "skills"       // skill listings (Claude Code)
	CtxAgents      = "agents"       // custom agent listings (Claude Code)
	CtxMCP         = "mcp"          // MCP tool definitions (Claude Code)
	CtxUser        = "user"         // the user's messages
	CtxAssistant   = "assistant"    // the model's replies
	CtxToolCalls   = "tool_calls"   // the model's tool calls' arguments
	CtxToolResults = "tool_results" // what tools returned
	CtxReasoning   = "reasoning"    // the model's reasoning / thinking
	CtxAttachments = "attachments"  // attached files and images
	CtxSummary     = "summary"      // the summary a compaction left
	CtxMessages    = "messages"     // messages, not split further
	CtxOther       = "other"
	CtxFree        = "free"   // room left before compaction
	CtxBuffer      = "buffer" // reserved: room the window has beyond the compaction point
)

// ContextCategory is one slice of the context. Exact means the provider
// itself counted it (Claude Code counts each category with the token-count
// API); otherwise it is yoakeland's character-based estimate, scaled so all
// the used slices add up to the provider's reported total when there is one.
type ContextCategory struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
	Exact  bool   `json:"exact"`
}

// ContextCache is what the last request read from and wrote to the prompt
// cache, out of Input (its whole prompt). Omitted when the provider gave no
// per-request figure.
type ContextCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
	Input int `json:"input"`
}

// ContextBreakdownData answers get_context_breakdown. Used categories come
// first, then free and buffer; WindowTokens, when known, is what they all add
// up to. TotalTokens is what the context holds now. Source says where the
// total came from ("provider": counted by the provider for its last request;
// "estimated": a character heuristic; "unknown": nothing to measure yet);
// each category's Exact says how its own figure was arrived at.
type ContextBreakdownData struct {
	RequestID string `json:"requestId,omitempty"`
	SessionID string `json:"sessionId"`
	Model     string `json:"model,omitempty"`

	TotalTokens  int `json:"totalTokens"`
	WindowTokens int `json:"windowTokens,omitempty"`
	// AutoCompactAt is the size at which the session compacts itself; 0 when
	// the agent does that on its own terms and doesn't say.
	AutoCompactAt int    `json:"autoCompactAt,omitempty"`
	Source        string `json:"source"`

	Categories []ContextCategory `json:"categories"`
	Cache      *ContextCache     `json:"cache,omitempty"`
	Spend      *SpendData        `json:"spend,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// SetModeData switches a session between the modes on its next turn —
// "read_only", "read_write", "accept_edits", "auto", "autonomous", or "plan" (see SPEC.md §4c).
// ApprovePlanData approves the plan a session has shown (SPEC.md §3.1
// approve_plan): the session moves to Mode and carries the plan out.
type ApprovePlanData struct {
	SessionID    string `json:"sessionId,omitempty"`
	Mode         string `json:"mode"`
	ProjectPath  string `json:"projectPath,omitempty"`
	AuthorName   string `json:"authorName,omitempty"`
	AuthorAvatar string `json:"authorAvatar,omitempty"`
}

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
	// Index (1-based) and Total place this question among those raised together,
	// so a client can say "2 of 3" however many have been answered; 0 when unset.
	Index int `json:"index,omitempty"`
	Total int `json:"total,omitempty"`
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

type GetUsageData struct {
	RequestID string `json:"requestId"`
	// External also reads the CLIs' own transcripts on the server's machine.
	External bool `json:"external,omitempty"`
	// Since drops buckets before this time (RFC 3339); empty means all.
	Since string `json:"since,omitempty"`
}

// UsageReportData lists usage per (hour, session, provider, model). Rows
// point into Sessions/Providers/Models by index to keep the payload small.
type UsageReportData struct {
	RequestID string          `json:"requestId"`
	Error     string          `json:"error,omitempty"`
	Sessions  []UsageSession  `json:"sessions"`
	Providers []UsageProvider `json:"providers"`
	Models    []string        `json:"models"`
	Rows      []UsageRow      `json:"rows"`
	// FirstAt is the oldest round-trip on record, regardless of Since.
	FirstAt string `json:"firstAt,omitempty"`
	// Deleted is spend of sessions deleted since, which has no timestamps
	// left, only totals (usage.json).
	Deleted UsageDeleted `json:"deleted"`
	// Accounts is what api_key providers' own account APIs report.
	Accounts []UsageAccount `json:"accounts"`
}

// UsageAccount is one provider's account as its API reports it. Amounts
// are in Currency (USD unless it says otherwise).
type UsageAccount struct {
	Provider          string        `json:"provider"`
	Currency          string        `json:"currency,omitempty"`
	Balance           *float64      `json:"balance,omitempty"`
	Usage             *UsagePeriods `json:"usage,omitempty"`
	LimitUSD          *float64      `json:"limitUsd,omitempty"`
	LimitRemainingUSD *float64      `json:"limitRemainingUsd,omitempty"`
	Error             string        `json:"error,omitempty"`
	FetchedAt         time.Time     `json:"fetchedAt"`
}

// UsagePeriods is spend over the provider's own (UTC) day/week/month.
type UsagePeriods struct {
	Day   float64 `json:"day"`
	Week  float64 `json:"week"`
	Month float64 `json:"month"`
	Total float64 `json:"total"`
}

type UsageSession struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	ProjectPath string `json:"projectPath"`
	Archived    bool   `json:"archived,omitempty"`
	// Source is empty for a yoakeland chat, else the CLI whose own
	// transcript it came from: "claude_code" or "codex".
	Source string `json:"source,omitempty"`
}

// UsageProvider is a provider name as stamped on messages; Kind is empty
// when that provider has since been removed.
type UsageProvider struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type UsageRow struct {
	Hour         int64   `json:"h"` // unix seconds, start of the UTC hour
	Session      int     `json:"s"`
	Provider     int     `json:"p"`
	Model        int     `json:"m"`
	Requests     int     `json:"n"`
	InputTokens  int     `json:"in"`
	CachedTokens int     `json:"cached,omitempty"`
	OutputTokens int     `json:"out"`
	CostUSD      float64 `json:"usd"`
	GenerationMs int64   `json:"ms"`
}

type UsageDeleted struct {
	Sessions     int     `json:"sessions"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
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
	// ProjectPath is the canonical root the session runs in. For a chat
	// (projectPath "@chat") it's the only way to learn its scratch directory.
	ProjectPath string `json:"projectPath,omitempty"`
	// WorkspaceBytes is the size of a chat's scratch directory (a "@chat"
	// listing only; capped, see ProjectRegistry.ListChats).
	WorkspaceBytes int64 `json:"workspaceBytes,omitempty"`
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

// ImportChatFilesData copies a project-less chat's workspace into a project
// (SPEC.md §4b "Moving a chat"). SessionID names the chat; Target is a project
// the server already knows.
type ImportChatFilesData struct {
	SessionID string `json:"sessionId"`
	Target    string `json:"target"`
	RequestID string `json:"requestId,omitempty"`
}

// ChatFilesImportedData answers it: the new directory's name inside Target
// (relative to it) and what was copied, or Error.
type ChatFilesImportedData struct {
	RequestID string `json:"requestId,omitempty"`
	SessionID string `json:"sessionId"`
	Target    string `json:"target,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Files     int    `json:"files,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Error     string `json:"error,omitempty"`
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
	// SandboxTier is set on the "@chat" listing only: the tier a chat's scratch
	// directory gets, so a client knows whether a new chat can safely default to Run.
	SandboxTier string `json:"sandboxTier,omitempty"`
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
	// ParentToolCallID marks a message from a subagent's own transcript,
	// listed just before the result of the subagent call that ran it.
	ParentToolCallID string `json:"parentToolCallId,omitempty"`
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

	// Compaction is set on a "compaction" message only (SPEC.md §4g).
	Compaction *CompactionInfo `json:"compaction,omitempty"`
}

// CompactionInfo describes a "compaction" history message: what kind it was
// and how full the context was before and after it.
type CompactionInfo struct {
	Kind         string `json:"kind"`
	TokensBefore int    `json:"tokensBefore"`
	TokensAfter  int    `json:"tokensAfter"`
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
	// Context and Spend are what the last turn_completed of this session
	// would report now, so a client that opens the session cold (reload,
	// second device) or switches branch has its meters without a turn.
	// Context is omitted for a session with no messages.
	Context *ContextData `json:"context,omitempty"`
	Spend   *SpendData   `json:"spend,omitempty"`
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
//
// Addr, TLSCert, TLSKey and WebRoot are optional: an omitted field keeps its
// persisted value, "" clears it. Changing one while the listener is running
// restarts it. They replace the old -listen host:port, -tls-cert, -tls-key
// and -web-root launch flags.
type SetRemoteAccessData struct {
	Enabled bool    `json:"enabled"`
	Addr    *string `json:"addr,omitempty"`
	TLSCert *string `json:"tlsCert,omitempty"`
	TLSKey  *string `json:"tlsKey,omitempty"`
	WebRoot *string `json:"webRoot,omitempty"`
}

// RemoteAccessSetData is the remote listener's persisted state: pushed on
// connect, and to every peer after set_remote_access. TLSCert/TLSKey/WebRoot
// are only filled in for a local-socket peer.
type RemoteAccessSetData struct {
	Enabled bool `json:"enabled"`
	// Addr is the TCP address the listener binds ("" means ":7890").
	Addr    string `json:"addr,omitempty"`
	TLSCert string `json:"tlsCert,omitempty"`
	TLSKey  string `json:"tlsKey,omitempty"`
	WebRoot string `json:"webRoot,omitempty"`
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

// RuntimeSettings are the server's tunables that used to be launch flags or
// hard-coded constants. One typed struct: persisted in server.json, pushed to
// every peer on connect and after each change (runtime_settings), and changed
// with set_runtime_settings. Adding a setting means adding a field here plus
// its default and clamp in cmd/yoakeland/runtime_settings.go.
//
// A value of 0 means the setting's default, or "unlimited" where noted. The
// server clamps out-of-range values rather than rejecting them; the
// runtime_settings reply carries what was actually stored.
//
// Every field is enforced: the limits and bash policy by the project registry,
// the sandbox/terminal caps by internal/sandbox, the pool caps by
// internal/provider, the pricing interval by the pricing cache and
// BrowserBlockCrossOrigin by the browser broker.
type RuntimeSettings struct {
	// MaxRoundsPerTurn pauses a turn to ask "continue?" after this many model
	// round-trips (0 = unlimited). Applies to yoakeland's own API-provider
	// loop; native CLI agents run their own loop and are never limited.
	MaxRoundsPerTurn int `json:"maxRoundsPerTurn"`
	// MaxSessionCostUSD pauses a turn (and refuses a new one) once a session
	// has spent this much (0 = unlimited).
	MaxSessionCostUSD float64 `json:"maxSessionCostUsd"`
	// DisableBashWhenUnsandboxed withholds bash and the terminal_* tools from
	// the model in any project whose sandbox tier is
	// local_restricted_fallback.
	DisableBashWhenUnsandboxed bool `json:"disableBashWhenUnsandboxed"`

	ExecTimeoutSec          int  `json:"execTimeoutSec"`          // default 60, 1-3600
	ExecOutputMaxBytes      int  `json:"execOutputMaxBytes"`      // default 1 MiB
	TerminalMemoryMaxMB     int  `json:"terminalMemoryMaxMB"`     // default 2048, >= 64
	TerminalPidsMax         int  `json:"terminalPidsMax"`         // default 512, >= 16
	TerminalReadMaxChars    int  `json:"terminalReadMaxChars"`    // default 12000
	TerminalHistoryKB       int  `json:"terminalHistoryKB"`       // default 256
	PoolIdleMinutes         int  `json:"poolIdleMinutes"`         // default 30
	PoolMaxLive             int  `json:"poolMaxLive"`             // 0 = unlimited
	PricingRefreshHours     int  `json:"pricingRefreshHours"`     // default 24
	BrowserBlockCrossOrigin bool `json:"browserBlockCrossOrigin"` // default true
}

// SetRuntimeSettingsData is a partial update: a JSON object holding only the
// RuntimeSettings keys to change (unknown keys are an error).
type SetRuntimeSettingsData = map[string]json.RawMessage

// SetDefaultProviderData picks the registered provider (and model) sessions
// use until they call set_provider themselves. An empty ProviderName clears
// it. Replaces the -provider/-model/-api-key/-base-url launch flags.
type SetDefaultProviderData struct {
	ProviderName string `json:"providerName"`
	Model        string `json:"model,omitempty"`
}

// DefaultProviderSetData is pushed on connect and to every peer after a change.
type DefaultProviderSetData struct {
	ProviderName string `json:"providerName,omitempty"`
	Model        string `json:"model,omitempty"`
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

// MediaInfoData answers read_media: Size/Mime for a recognized media
// type under its size cap, or Error (unrecognized type, too
// large, not found). No Version/conflict handling — media is previewed,
// never edited through this path.
type MediaInfoData struct {
	RequestID string `json:"requestId,omitempty"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Mime      string `json:"mime,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MediaChunkRequestData asks for one bounded slice of a file already
// described by media_info, starting at Offset.
type MediaChunkRequestData struct {
	RequestID   string `json:"requestId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	Path        string `json:"path"`
	Offset      int64  `json:"offset"`
}

// MediaChunkData answers read_media_chunk. Last marks the final slice, so
// the client knows to stop asking without needing Size again.
type MediaChunkData struct {
	RequestID string `json:"requestId,omitempty"`
	Data      []byte `json:"data,omitempty"` // base64 on the wire
	Offset    int64  `json:"offset"`
	Last      bool   `json:"last,omitempty"`
	Error     string `json:"error,omitempty"`
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
	// NativeSessions is the pool of live CLI-agent processes (one per chat
	// and agent kind), bounded by the poolIdleMinutes / poolMaxLive settings.
	NativeSessions *NativeSessionsStatus `json:"nativeSessions,omitempty"`
}

// NativeSessionsStatus describes the pool of live CLI-agent processes.
type NativeSessionsStatus struct {
	Live           int            `json:"live"`
	Busy           int            `json:"busy"`
	Max            int            `json:"max,omitempty"` // 0 = no cap
	IdleTimeoutSec int            `json:"idleTimeoutSec"`
	ByKind         map[string]int `json:"byKind,omitempty"`
	OldestIdleSec  int            `json:"oldestIdleSec,omitempty"`
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
	ID    string `json:"id"`
	Level string `json:"level"` // "info" | "success" | "warning" | "error": the icon
	// Priority says how loudly to raise it: "urgent" (needs you: an ask, an
	// error, a spend cap), "normal" (worth knowing: a long turn finished) or
	// "quiet" (bell only: a device connected).
	Priority string `json:"priority"`
	Kind     string `json:"kind"` // e.g. "turn_done", "permission_request", "peer_joined"
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	// Count is how many times this happened while it sat unread (a repeat
	// within a minute replaces the entry instead of adding one). 1 when omitted.
	Count       int    `json:"count,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	// ToolCallID names the pending ask a permission_request /
	// ask_user_question_requested entry mirrors; it is retracted (dismissed)
	// when that ask is answered or cancelled.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Read is server-side, so it follows you across devices. An entry raised
	// for the chat someone was already looking at is born read.
	Read bool `json:"read,omitempty"`
	// Attended: a device was in use (window focused) when this was raised, so
	// the others should not add an OS-level alert on top.
	Attended  bool      `json:"attended,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
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

// ListAuditData asks for a page of the audit log (SPEC.md §3.1 list_audit),
// newest first. Before is the Seq of the last event of the previous page.
type ListAuditData struct {
	RequestID string   `json:"requestId,omitempty"`
	Before    int64    `json:"before,omitempty"`
	Limit     int      `json:"limit,omitempty"` // default 100, at most 500
	Kinds     []string `json:"kinds,omitempty"`
	SessionID string   `json:"sessionId,omitempty"`
	Project   string   `json:"projectPath,omitempty"`
	Since     string   `json:"since,omitempty"` // RFC 3339
}

// AuditListData answers list_audit. More says older matches remain: ask
// again with Before set to the last event's Seq.
type AuditListData struct {
	RequestID string        `json:"requestId,omitempty"`
	Events    []audit.Event `json:"events"`
	More      bool          `json:"more"`
	Error     string        `json:"error,omitempty"`
}

// MarkNotificationsReadData marks the listed notifications read, or every
// one when IDs is empty. Echoed back verbatim as notifications_read to every
// peer.
type MarkNotificationsReadData struct {
	IDs []string `json:"ids,omitempty"`
}

// NotifyPrefsData is the server-wide notification policy (set_notify_prefs
// in, notify_prefs out). Urgent/Normal/Quiet switch a whole priority tier;
// Muted silences single kinds inside a tier that is on. MinTurnSec is how
// long a turn must have run before its "response ready" is worth raising.
type NotifyPrefsData struct {
	Urgent     bool     `json:"urgent"`
	Normal     bool     `json:"normal"`
	Quiet      bool     `json:"quiet"`
	MinTurnSec int      `json:"minTurnSec"`
	Muted      []string `json:"muted"`
}

// SetPresenceData is a device's heartbeat: which chats it shows (the main
// one and the sidebar's) and whether its window is focused and visible. The
// server treats one older than five minutes as gone.
type SetPresenceData struct {
	SessionIDs []string `json:"sessionIds,omitempty"`
	Focused    bool     `json:"focused"`
	Visible    bool     `json:"visible"`
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

// ChatProject is the projectPath that binds a session to its own scratch
// directory on the server instead of a project: a chat with no project. As
// list_sessions' projectPath it lists every such chat.
const ChatProject = "@chat"

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

// SteerTurnData puts a message into a turn that is still running (§3.1
// steer_turn). Only a CLI agent that can take one mid-turn accepts it.
type SteerTurnData struct {
	SessionID   string `json:"sessionId,omitempty"`
	TurnID      string `json:"turnId"` // the running turn
	Prompt      string `json:"prompt"`
	ProjectPath string `json:"projectPath,omitempty"`

	AuthorName   string `json:"authorName,omitempty"`
	AuthorAvatar string `json:"authorAvatar,omitempty"`
}

// SteerResultData is steer_accepted (Prompt is now a message in the turn, MessageID
// its id) or steer_rejected (Reason says why; send it as an ordinary prompt instead).
type SteerResultData struct {
	Prompt    string `json:"prompt"`
	MessageID string `json:"messageId,omitempty"`
	Reason    string `json:"reason,omitempty"`
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
	// ParentToolCallID: the agent's own subagent call (Claude Code's Task)
	// this call ran inside of, empty for a top-level call.
	ParentToolCallID string `json:"parentToolCallId,omitempty"`
}

type PermissionRequestData struct {
	ToolName    string         `json:"toolName"`
	Description string         `json:"description"`
	RequiresAsk bool           `json:"requiresAsk"`
	CacheKey    string         `json:"cacheKey"`
	Diff        *diff.FileDiff `json:"diff,omitempty"` // For rich UI diff previews
	// Plan: the agent is asking to leave plan mode with the plan a
	// `plan_written` just showed; approve it with approve_plan, or answer
	// with feedback to send it back for changes.
	Plan bool `json:"plan,omitempty"`
	// Suggestions are rules the user may pick instead of a bare "always
	// allow" (answer with permission_response.ruleId). For a CLI agent's ask
	// they are the agent's own "always allow" options (Native: true).
	Suggestions []PermissionSuggestion `json:"suggestions,omitempty"`
}

// PermissionSuggestion is one "stop asking about this" option on a
// permission_request. The server rebuilds the rule from its own copy when
// the answer names ID, so a client never supplies a pattern.
type PermissionSuggestion struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Tool    string   `json:"tool,omitempty"`
	Pattern []string `json:"pattern,omitempty"`
	// Scope is the default scope offered: session | project | global.
	Scope  string `json:"scope,omitempty"`
	Native bool   `json:"native,omitempty"`
}

type PermissionResponseData struct {
	Allowed  bool   `json:"allowed"`
	Remember bool   `json:"remember"`
	Feedback string `json:"feedback,omitempty"`
	// RuleID picks one of the request's suggestions to remember instead of
	// the plain exact-match Remember; Scope overrides where it is stored
	// (session | project | global). Both are ignored without an allow/deny.
	RuleID string `json:"ruleId,omitempty"`
	Scope  string `json:"scope,omitempty"`
}

// PermissionRuleData is one permission rule on the wire (§3.1 "Permission rules").
type PermissionRuleData struct {
	ID        string   `json:"id,omitempty"`
	Tool      string   `json:"tool"`
	Pattern   []string `json:"pattern"`
	Text      string   `json:"text,omitempty"` // the pattern as typed, for display; read on add if Pattern is empty
	Decision  string   `json:"decision"`
	Scope     string   `json:"scope,omitempty"`
	Modes     []string `json:"modes,omitempty"`
	Source    string   `json:"source,omitempty"`
	CreatedAt string   `json:"createdAt,omitempty"`
	Match     []string `json:"match,omitempty"`
	NotMatch  []string `json:"notMatch,omitempty"`
	// Readonly marks a built-in preset.
	Readonly bool `json:"readonly,omitempty"`
}

type ListPermissionRulesData struct {
	ProjectPath string `json:"projectPath,omitempty"`
}

type AddPermissionRuleData struct {
	ProjectPath string             `json:"projectPath,omitempty"`
	Scope       string             `json:"scope"`
	Rule        PermissionRuleData `json:"rule"`
}

type DeletePermissionRuleData struct {
	ProjectPath string `json:"projectPath,omitempty"`
	ID          string `json:"id"`
}

// PermissionRulesData answers the three rule requests, and is broadcast to
// every peer when rules change. Errors are rule files that failed to load
// (approvals ask until fixed); Notes are v1 entries migration dropped.
type PermissionRulesData struct {
	ProjectPath string               `json:"projectPath"`
	Rules       []PermissionRuleData `json:"rules"`
	Errors      []string             `json:"errors,omitempty"`
	Notes       []string             `json:"notes,omitempty"`
	// Error is why the request that produced this reply was refused.
	Error string `json:"error,omitempty"`
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
	// CachedInputTokens is the part of InputTokens read from the provider's
	// prompt cache, when it reports one; CacheWriteInputTokens the part
	// written to it (billed above the plain input rate by providers that
	// charge for writes).
	CachedInputTokens     int `json:"cachedInputTokens,omitempty"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`

	// ContextTokens is how many tokens the context held after the last
	// model request this usage covers (that request's input plus its
	// reply); WindowTokens is the model's context window, when the provider
	// says. Unlike the counts above they are not sums: a native agent's
	// turn makes many requests, and only the last one's size says how full
	// the context is. Zero when the provider reports neither.
	ContextTokens int `json:"contextTokens,omitempty"`
	WindowTokens  int `json:"windowTokens,omitempty"`

	// CostKnown marks CostUSD as settled: reported by the provider itself
	// (e.g. a CLI subprocess's own billing total) or priced from a rate
	// table, as opposed to the zero value. It controls whether the agent
	// prices this usage again, since a bare CostUSD==0 can't distinguish
	// "provider hasn't reported a cost" from "provider reported a genuine
	// $0". On a turn's total it means every part of the total was priced.
	CostKnown bool `json:"costKnown,omitempty"`
	// CostUnknown marks usage no rate exists for: CostUSD is 0 (or, on a
	// total, only the priced part) but the real cost is not.
	CostUnknown bool `json:"costUnknown,omitempty"`
}

// ContextData reports how full the model's context window is after a turn,
// so a frontend can render a context meter.
type ContextData struct {
	EstimatedTokens int `json:"estimatedTokens"`
	// MaxTokens is the meter's denominator: the model's context window when
	// known, else BudgetTokens.
	MaxTokens int `json:"maxTokens"`
	// WindowTokens is the model's real context window; omitted when unknown.
	WindowTokens int `json:"windowTokens,omitempty"`
	// BudgetTokens is the size at which the session compacts itself: the
	// configured budget, clamped to the window and never below the minimum.
	BudgetTokens int `json:"budgetTokens,omitempty"`
	// Source is where EstimatedTokens came from: "provider" (a count the
	// provider reported for its last request), "estimated" (a character
	// heuristic), or "unknown" (nothing in the context yet to measure).
	Source string `json:"source,omitempty"`
}

// SpendData reports running cost totals, persisted across process restarts.
type SpendData struct {
	SessionTotalUSD float64 `json:"sessionTotalUsd"`
	ProjectTotalUSD float64 `json:"projectTotalUsd"`
	// SessionCostUnknown / ProjectCostUnknown: some usage counted in the
	// total had no rate, so the true total is higher than shown.
	SessionCostUnknown bool `json:"sessionCostUnknown,omitempty"`
	ProjectCostUnknown bool `json:"projectCostUnknown,omitempty"`
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
	// Name/Agent: see TerminalSummary.
	Name  string `json:"name,omitempty"`
	Agent bool   `json:"agent,omitempty"`
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
	// Name is what the agent called a terminal it started (terminal_start),
	// and Agent marks one it started — a client shows it like any other tab.
	Name  string `json:"name,omitempty"`
	Agent bool   `json:"agent,omitempty"`
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
	return s.sendEvent(evtType, sessionID, turnID, toolCallID, "", payload)
}

func (s *IPCStream) sendEvent(evtType EventType, sessionID, turnID, toolCallID, parentToolCallID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	return s.Send(&Envelope{
		Type:             evtType,
		SessionID:        sessionID,
		TurnID:           turnID,
		ToolCallID:       toolCallID,
		ParentToolCallID: parentToolCallID,
		Data:             raw,
	})
}

// BrowserHostData is a client offering (or withdrawing) its browser panel
// for the agent's browser tool — sent by a desktop client after connecting.
type BrowserHostData struct {
	Enabled bool `json:"enabled"`
}

// BrowserRequestData asks the hosting client to perform one browser action
// in SessionID's browser panel. Args are the browser tool's own arguments.
type BrowserRequestData struct {
	RequestID   string          `json:"requestId"`
	SessionID   string          `json:"sessionId"`
	ProjectPath string          `json:"projectPath,omitempty"`
	Action      string          `json:"action"`
	Args        json.RawMessage `json:"args"`
	// AllowedHosts are the hostnames the agent may end up on in this chat:
	// the ones it navigated to (each passed the permission check) plus
	// loopback. With BlockCrossOrigin set, a host that enforces it must stop
	// any other main-frame navigation an agent action causes (a click on an
	// off-site link, a redirect) and tell the agent to use navigate, which
	// asks the user. Old clients ignore both fields.
	AllowedHosts     []string `json:"allowedHosts,omitempty"`
	BlockCrossOrigin bool     `json:"blockCrossOrigin,omitempty"`
}

// BrowserResponseData answers one browser_request: Output is the text the
// agent gets back (a page snapshot, an evaluate result), or Error.
type BrowserResponseData struct {
	RequestID string `json:"requestId"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}
