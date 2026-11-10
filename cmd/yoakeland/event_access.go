package main

import (
	"fmt"
	"strings"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// eventReq is what an inbound event needs from a TCP connection. The local socket and -stdio are
// the owner and need nothing.
type eventReq struct {
	min     string // lowest tier: "" for any, TierTrusted, TierFull, or tierLocal (the local socket only)
	cap     string // a capability required below the Full tier
	ownChat bool   // the connection must own the chat the event is about (ownsChat)
}

const tierLocal = "local"

var (
	anyone  = eventReq{}
	trusted = eventReq{min: TierTrusted}
	full    = eventReq{min: TierFull}
	own     = eventReq{ownChat: true}
)

func needCap(c string) eventReq { return eventReq{cap: c} }

// eventAccess classifies every event a client can send (PLAN_PERMISSIONS "Event access"). It is
// the only place that says who may send what. An event missing from it is refused below the Full
// tier, so a new one is owner-only until someone lists it here; TestEveryInboundEventIsClassified
// fails until they do. Rules a table row can't say (which mode, which provider, whose schedule)
// are checked in the handler.
var eventAccess = map[protocol.EventType]eventReq{
	// chat: the sender's own chats
	protocol.EventUserPrompt: own, protocol.EventEditMessage: own, protocol.EventRetryMessage: own,
	protocol.EventSwitchBranch: own, protocol.EventCompactContext: own, protocol.EventSideQuestion: own,
	protocol.EventSummarizeMessage: own, protocol.EventSteerTurn: own, protocol.EventCancelTurn: own,
	protocol.EventCancelSubagent: own, protocol.EventPermissionResponse: own, protocol.EventAskUserAnswer: own,
	protocol.EventApprovePlan: own, protocol.EventLimitContinue: own, protocol.EventLimitDismiss: own,
	protocol.EventArtifactEvent:     own,
	protocol.EventGetSessionHistory: own, protocol.EventGetContextBreakdown: own, protocol.EventSetSessionTitle: own,
	protocol.EventSetSessionArchived: own, protocol.EventSetSessionLayout: own, protocol.EventDeleteSession: own,
	protocol.EventListSnapshots: own, protocol.EventRestoreSnapshot: {cap: CapFilesEdit, ownChat: true}, // and the snapshot must be the chat's: the handler
	protocol.EventReadMedia: anyone, protocol.EventReadMediaChunk: anyone, // project media is open; an attachment needs its chat: the handler
	protocol.EventListSessions:       anyone,  // filtered to the sender's chats by the handler
	protocol.EventSetMode:            own,     // and the mode itself is checked by the handler
	protocol.EventSetWebToolsEnabled: trusted, // saves a project-wide setting
	protocol.EventSetModelOptions:    own,
	protocol.EventSetProvider:        own, // API-key providers or the ones listed in the cli cap: the handler

	// models, projects and files
	protocol.EventSetFavoriteModels: anyone, protocol.EventListProviders: anyone, protocol.EventListModels: anyone,
	protocol.EventListProjects: anyone, protocol.EventOpenProject: anyone, // known projects only, for anyone below Full
	protocol.EventListTree: anyone, protocol.EventReadFile: anyone, protocol.EventListFiles: anyone,
	protocol.EventListChanges: anyone, protocol.EventGetFileDiff: anyone, protocol.EventListPermissionRules: anyone,
	protocol.EventUploadBegin: anyone, protocol.EventUploadChunk: anyone, protocol.EventImportChatFiles: own, // the files of a chat: its owner's
	protocol.EventWriteFile: needCap(CapFilesEdit), protocol.EventFsOp: needCap(CapFilesEdit),

	// model forwarding: a device relays between two servers; the handlers narrow (forward.go)
	protocol.EventRelayOffer: anyone, protocol.EventForwardRequest: anyone, protocol.EventForwardCancel: anyone,
	protocol.EventForwardChunk: anyone, protocol.EventForwardEnd: anyone,

	// presence and housekeeping
	protocol.EventSetPresence: anyone, protocol.EventSetPeerName: anyone, protocol.EventSetPeerAvatar: anyone, protocol.EventSetProfile: anyone, protocol.EventGetAvatar: anyone,
	protocol.EventSetNotifyPrefs: anyone, protocol.EventListNotifications: anyone, protocol.EventDismissNotification: anyone,
	protocol.EventMarkNotificationsRead: anyone, protocol.EventPing: anyone, protocol.EventPong: anyone,
	protocol.EventAuth:          anyone,                // only meaningful as the first frame; later it falls to "unknown event"
	protocol.EventGetUsage:      anyone,                // everyone's spend, no external sessions, below Full: the handler
	protocol.EventListProcesses: needCap(CapProcesses), // `command` is stripped below Full by the handler
	protocol.EventListSkills:    anyone, protocol.EventGetSkill: anyone,

	// terminals and the server's browser can be watched with a cap
	protocol.EventListTerminals: needCap(CapTerminalsWatch), protocol.EventTerminalAttach: needCap(CapTerminalsWatch),
	protocol.EventTerminalDetach: needCap(CapTerminalsWatch), protocol.EventTerminalResize: needCap(CapTerminalsWatch),
	protocol.EventBrowserView: needCap(capBrowserWatch), protocol.EventBrowserFrameAck: needCap(capBrowserWatch),

	// helping run the server
	protocol.EventSetSkillEnabled: trusted, protocol.EventSetServerIdentity: trusted, protocol.EventSetAutoGenerateTitle: trusted,
	protocol.EventRestartServer: trusted, protocol.EventScheduleTurn: trusted, protocol.EventListScheduled: trusted,
	protocol.EventCancelScheduled: trusted, protocol.EventPauseScheduled: trusted, protocol.EventSignalProcess: trusted,
	protocol.EventListProblems: trusted, protocol.EventReadLog: trusted,
	protocol.EventRegisterProvider: trusted, // new, API-key providers only for a Trusted sender: the handler
	// people and pairing: what a Trusted sender may do is narrowed by the handler
	protocol.EventListTokens: trusted, protocol.EventGetPairInfo: trusted, protocol.EventRegisterToken: trusted,
	protocol.EventRemoveToken: trusted, protocol.EventApprovePairing: trusted,

	// people: what a Trusted sender may do is narrowed by the handler (their own and the people they added)
	protocol.EventListPeople: trusted,

	// owner powers
	protocol.EventSetUserPermissions: full, protocol.EventSetDeviceRestrict: full, protocol.EventSetLimitedDefaults: full,
	protocol.EventMergeUsers: full, protocol.EventMoveDevice: full, protocol.EventRemoveUser: full,
	protocol.EventRemoveProvider: full, protocol.EventSetDefaultProvider: full, protocol.EventOAuthStart: full,
	protocol.EventOAuthComplete: full, protocol.EventSetModelRoles: full, protocol.EventDetectCLI: full,
	protocol.EventListACPRegistry: full, protocol.EventDisconnectPeer: full, protocol.EventSetPeerScope: full,
	protocol.EventAddPermissionRule: full, protocol.EventDeletePermissionRule: full,
	protocol.EventListMCPServers: full, protocol.EventSaveMCPServer: full, protocol.EventRemoveMCPServer: full,
	protocol.EventSetMCPServerEnabled: full, protocol.EventSetMCPToolEnabled: full, protocol.EventReconnectMCPServer: full,
	protocol.EventMCPOAuthStart: full, protocol.EventMCPOAuthComplete: full, protocol.EventMCPSignOut: full,
	protocol.EventScanMCPImports: full, protocol.EventImportMCPServers: full,
	protocol.EventSetRuntimeSettings: full, protocol.EventSetLimitAction: full, protocol.EventListAudit: full,
	protocol.EventListDirectory: full, protocol.EventRemoveProject: full,
	protocol.EventTerminalOpen: full, protocol.EventTerminalInput: full, protocol.EventTerminalClose: full,
	protocol.EventBrowserControl: full, protocol.EventBrowserInput: full, protocol.EventBrowserNav: full,
	protocol.EventBrowserHost: full, protocol.EventBrowserResponse: full,

	// settings that open the door to the machine stay on the local socket
	protocol.EventSetRemoteAccess: {min: tierLocal}, protocol.EventSetRemotePassword: {min: tierLocal},
}

// checkEvent decides whether the connection with acc may send env. A refusal names what is missing.
// The local socket and -stdio (acc.Local) may send anything; the handler answers an unknown event.
func checkEvent(deps connDeps, acc Access, env *protocol.Envelope) (ok bool, why string) {
	if acc.Local {
		return true, ""
	}
	t := env.Type
	req, known := eventAccess[t]
	switch {
	case req.min == tierLocal:
		return false, fmt.Sprintf("%s is only available on the computer running the server", t)
	case acc.Full():
		return true, ""
	case !known:
		return false, tierNeeded(t, TierFull)
	case req.min != "" && !acc.AtLeast(req.min):
		return false, tierNeeded(t, req.min)
	case req.cap != "" && !acc.Caps.Has(req.cap):
		return false, fmt.Sprintf("%s isn't allowed for this person", t)
	case req.ownChat && !ownsEnvelope(deps, acc, env):
		return false, fmt.Sprintf("%s isn't allowed for this person: it isn't their chat", t)
	}
	return true, ""
}

func tierNeeded(t protocol.EventType, tier string) string {
	return fmt.Sprintf("%s needs %s access", t, strings.ToUpper(tier[:1])+tier[1:])
}

// remoteDenied reports whether the connection peerID on network may not send t at all: whose chat
// it is aside, which depends on the envelope.
func remoteDenied(deps connDeps, network, peerID string, t protocol.EventType) bool {
	acc := deps.accessFor(network, peerID)
	acc.Caps.OthersChats = true
	ok, _ := checkEvent(deps, acc, &protocol.Envelope{Type: t})
	return !ok
}

// modeAllowed reports whether acc may put a chat in mode. Autonomous and auto need the autonomous
// cap, and without files_edit only read_only and plan are open.
func modeAllowed(acc Access, mode agent.Mode) bool {
	if acc.Full() {
		return true
	}
	if (mode == agent.ModeAutonomous || mode == agent.ModeAuto) && !acc.Caps.Autonomous {
		return false
	}
	return acc.Caps.FilesEdit || mode == agent.ModeReadOnly || mode == agent.ModePlan
}

// providerAllowed reports whether acc may bind a chat to the provider registered as name: any
// API-key provider, or a CLI agent listed in its cli cap. An unknown name is left to the handler.
func providerAllowed(deps connDeps, acc Access, name string) bool {
	// A provider borrowed from another server runs nothing here under the owner's login; the
	// server that has it decides what this person may use (forward.go).
	if _, _, via := parseVia(name); via || acc.Full() || deps.providerStore == nil {
		return true
	}
	cfg, ok := deps.providerStore.Get(name)
	if !ok {
		return true
	}
	if _, cli := provider.CLIKindOf(cfg.Kind); !cli {
		return true
	}
	return acc.Caps.AllowCLI(name)
}

// registerProviderRefusal says why acc may not register cfg, or "". A Trusted sender may add new
// API-key providers and change only the ones they added: a CLI agent or a custom command would run
// on this computer as the owner, which only Full may set up.
func registerProviderRefusal(deps connDeps, acc Access, cfg provider.ProviderConfig) string {
	if strings.HasPrefix(cfg.Name, viaPrefix) {
		return `a provider's name can't start with "via:": that is how a provider borrowed from another server is named`
	}
	if acc.Full() {
		return ""
	}
	if _, cli := provider.CLIKindOf(cfg.Kind); cli || cfg.Command != "" || len(cfg.Args) > 0 || cfg.ConfigDir != "" || len(cfg.Env) > 0 {
		return "register_provider for a CLI agent or a custom command needs Full access"
	}
	if _, exists := deps.providerStore.Get(cfg.Name); exists && deps.settings.ProviderOwner(cfg.Name) != acc.UserID {
		return fmt.Sprintf("provider %q already exists and wasn't added by this person", cfg.Name)
	}
	return ""
}
