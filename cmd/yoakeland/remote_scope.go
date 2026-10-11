package main

import "yoakeland/internal/protocol"

// remoteAllowed is everything a restricted remote device (a paired phone) may ask for. It is
// an allowlist on purpose: an event added later is owner-only until someone lists it here.
// Left out, so they need the local socket or an unrestricted credential: providers and their
// keys, MCP servers, skills, runtime and server settings, permission-rule changes, tokens,
// terminal_open/terminal_input, list_directory, remove_project, disconnect_peer, the audit log.
var remoteAllowed = eventSet(
	// chat
	protocol.EventUserPrompt, protocol.EventEditMessage, protocol.EventRetryMessage, protocol.EventSwitchBranch,
	protocol.EventCompactContext, protocol.EventPermissionResponse, protocol.EventCancelTurn, protocol.EventSteerTurn,
	protocol.EventRestoreSnapshot, protocol.EventSetMode, protocol.EventApprovePlan, protocol.EventSetWebToolsEnabled,
	protocol.EventAskUserAnswer, protocol.EventGetSessionHistory, protocol.EventGetContextBreakdown,
	protocol.EventListSessions, protocol.EventSetSessionTitle, protocol.EventSetSessionArchived,
	protocol.EventSetSessionLayout, protocol.EventDeleteSession, protocol.EventListSnapshots,
	// models and projects (open_project is already limited to projects the server has open)
	protocol.EventListProviders, protocol.EventListModels, protocol.EventSetFavoriteModels, protocol.EventSetProvider,
	protocol.EventSetModelOptions, protocol.EventSetModelRoles, protocol.EventOpenProject, protocol.EventListProjects,
	// files (handleFileEnvelope)
	protocol.EventListTree, protocol.EventReadFile, protocol.EventWriteFile, protocol.EventFsOp, protocol.EventListFiles,
	protocol.EventListChanges, protocol.EventGetFileDiff, protocol.EventReadMedia, protocol.EventReadMediaChunk,
	protocol.EventUploadBegin, protocol.EventUploadChunk, protocol.EventImportChatFiles,
	// read-only views
	protocol.EventListPermissionRules, protocol.EventListNotifications, protocol.EventGetUsage,
	// presence and housekeeping
	protocol.EventPing, protocol.EventPong, protocol.EventSetPresence, protocol.EventSetPeerName,
	protocol.EventSetPeerAvatar, protocol.EventDismissNotification, protocol.EventMarkNotificationsRead,
	protocol.EventSetNotifyPrefs,
	// a terminal the owner opened can be watched, but not typed into or created
	protocol.EventListTerminals, protocol.EventTerminalAttach, protocol.EventTerminalResize,
)

func eventSet(types ...protocol.EventType) map[protocol.EventType]bool {
	m := make(map[protocol.EventType]bool, len(types))
	for _, t := range types {
		m[t] = true
	}
	return m
}

// remoteDenied reports whether the peer behind a tcp connection may not send t.
func remoteDenied(deps connDeps, network, peerID string, t protocol.EventType) bool {
	if network != "tcp" || remoteAllowed[t] {
		return false
	}
	return deps.peers.IsRestricted(peerID)
}
