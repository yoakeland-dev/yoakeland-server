package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"yoakeland/internal/agent"
	"yoakeland/internal/projectstore"
	"yoakeland/internal/protocol"
)

// Chat ownership (PLAN_PERMISSIONS "Chats"). A session stores the user who created it; one with no
// owner belongs to the owner. Everything below fails closed: an unreadable session, an unknown
// project or a missing registry is refused, and only a session that doesn't exist yet is the
// sender's to create.

// maxOwnerCache bounds the id -> owner memory below.
const maxOwnerCache = 8192

// rememberOwner records who owns sessionID, so the many events of a streaming turn don't each
// read the session file. An owner never changes, so the entry is kept until the process ends;
// a chat that doesn't exist yet is remembered as its creator's, which also lets that person's
// other devices see its first events.
// ponytail: bounded by dropping new entries, not by evicting; an LRU if a server holds more
// than maxOwnerCache chats at once.
func (r *ProjectRegistry) rememberOwner(sessionID, ownerUser string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners == nil {
		r.owners = map[string]string{}
	}
	if _, ok := r.owners[sessionID]; ok || len(r.owners) < maxOwnerCache {
		r.owners[sessionID] = ownerUser
	}
}

func (r *ProjectRegistry) cachedOwner(sessionID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.owners[sessionID]
	return o, ok
}

// stateOwner is the user a stored session belongs to.
func stateOwner(st *agent.SessionState) string {
	if st.OwnerID == "" {
		return ownerID
	}
	return st.OwnerID
}

var errNoSessionProject = errors.New("the chat's project isn't known")

// sessionStoreFor is the session store of the project at key (a projectKey) without opening the
// project. A key the server doesn't know, or a chat whose folder isn't there, has none.
func (r *ProjectRegistry) sessionStoreFor(key string) (agent.SessionStore, error) {
	r.mu.Lock()
	pc, open := r.projects[key]
	r.mu.Unlock()
	if open {
		return pc.SessStore, nil
	}
	if r.isChat(key) {
		if _, err := os.Stat(key); err != nil {
			return nil, err
		}
	} else if !r.Known(key) {
		return nil, errNoSessionProject
	}
	return agent.NewDiskSessionStore(projectstore.DataDir(r.stateDir, key))
}

// sessionKey is the project a handler would resolve (sessionID, explicitPath) to, worked out the
// way Resolve does but without binding anything: the explicit path, else the session's binding,
// else the server's default project. An explicit path that contradicts the binding has no answer,
// as it has none for the handler.
func (r *ProjectRegistry) sessionKey(sessionID, explicitPath, defaultRoot string) (string, error) {
	r.mu.Lock()
	bound, isBound := r.sessionProject[sessionID]
	r.mu.Unlock()
	switch {
	case explicitPath == protocol.ChatProject:
		if !chatIDPattern.MatchString(sessionID) {
			return "", errNoSessionProject
		}
		key := filepath.Join(r.chatsDir, sessionID)
		if isBound && bound != key {
			return "", errNoSessionProject
		}
		return key, nil
	case explicitPath != "":
		key, err := projectKey(explicitPath)
		if err != nil || isBound && bound != key {
			return "", errNoSessionProject
		}
		return key, nil
	case isBound:
		return bound, nil
	case defaultRoot != "":
		return projectKey(defaultRoot)
	}
	return "", errNoSessionProject
}

// sessionOwnerAt is who owns sessionID as the handler for (explicitPath, defaultRoot) would find
// it. found is false for a session that doesn't exist yet.
func (r *ProjectRegistry) sessionOwnerAt(sessionID, explicitPath, defaultRoot string) (owner string, found bool, err error) {
	if o, ok := r.cachedOwner(sessionID); ok {
		return o, true, nil
	}
	key, err := r.sessionKey(sessionID, explicitPath, defaultRoot)
	if err != nil {
		return "", false, err
	}
	store, err := r.sessionStoreFor(key)
	if err != nil {
		if r.isChat(key) && os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	st, err := store.Load(sessionID)
	switch {
	case errors.Is(err, agent.ErrSessionNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	o := stateOwner(st)
	r.rememberOwner(sessionID, o)
	return o, true, nil
}

// ownerOfSession is who owns sessionID wherever it lives, for deciding who may be told about it.
// ok is false when there is no such chat, which nobody below Full may see.
func (r *ProjectRegistry) ownerOfSession(sessionID string) (owner string, ok bool) {
	if r == nil || sessionID == "" {
		return "", false
	}
	if o, hit := r.cachedOwner(sessionID); hit {
		return o, true
	}
	keys := r.List()
	if chatRoot := filepath.Join(r.chatsDir, sessionID); chatIDPattern.MatchString(sessionID) {
		keys = append(keys, chatRoot)
	}
	r.mu.Lock()
	if bound, isBound := r.sessionProject[sessionID]; isBound {
		keys = append([]string{bound}, keys...)
	}
	r.mu.Unlock()
	for _, key := range keys {
		store, err := r.sessionStoreFor(key)
		if err != nil {
			continue
		}
		if st, err := store.Load(sessionID); err == nil {
			o := stateOwner(st)
			r.rememberOwner(sessionID, o)
			return o, true
		}
	}
	return "", false
}

// same reports whether the user ids a and b are one person; without settings wired in, only
// equal ids are.
func (r *ProjectRegistry) same(a, b string) bool {
	if r == nil {
		return a == b
	}
	r.mu.Lock()
	f := r.sameUser
	r.mu.Unlock()
	if f == nil {
		return a == b
	}
	return f(a, b)
}

// SameUser reports whether the user ids a and b are one person, following merges. A person who no
// longer exists is only themselves.
func (s *ServerSettings) SameUser(a, b string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ua, okA := s.resolveUserLocked(a)
	ub, okB := s.resolveUserLocked(b)
	if okA && okB {
		return ua.ID == ub.ID
	}
	return a == b
}

// mayAct reports whether acc may act on a chat owned by ownerUser.
func (d connDeps) mayAct(acc Access, ownerUser string) bool {
	return acc.Local || acc.Full() || acc.Caps.OthersChats || d.settings != nil && acc.UserID != "" && d.settings.SameUser(ownerUser, acc.UserID)
}

// ownsChat reports whether the connection may act on the chat sessionID, which the sender names
// together with projectPath: it owns it, has the others_chats cap, or is Full. A chat that doesn't
// exist yet is the sender's to create (it is claimed for them, so their other devices see it).
// Anything that can't be read is refused.
func ownsChat(deps connDeps, acc Access, sessionID, projectPath string) bool {
	if acc.Local || acc.Full() || acc.Caps.OthersChats {
		return true
	}
	if deps.projects == nil || deps.settings == nil || acc.UserID == "" {
		return false
	}
	owner, found, err := deps.projects.sessionOwnerAt(sessionID, projectPath, deps.defaultProjectRoot)
	switch {
	case err != nil:
		return false
	case !found && reservedChatIDs[sessionID]:
		return false // "default" is what handlers fall back to; until the owner makes it, it is theirs
	case !found:
		deps.projects.rememberOwner(sessionID, acc.UserID)
		return true
	}
	return deps.mayAct(acc, owner)
}

// mayViewChat reports whether acc may be told about events of the chat sessionID.
func (d connDeps) mayViewChat(acc Access, sessionID string) bool {
	if acc.Local || acc.Full() || acc.Caps.OthersChats {
		return true
	}
	owner, ok := d.projects.ownerOfSession(sessionID)
	return ok && d.mayAct(acc, owner)
}

// reservedChatIDs are session ids the server itself falls back to. Nobody below Full can claim one
// by naming it first: while it doesn't exist it is the owner's.
var reservedChatIDs = map[string]bool{"default": true}

// projectLevelChatEvents act on a project, not on one chat, when they name no chat; the others
// fall back to the "default" chat as their handlers do.
var projectLevelChatEvents = map[protocol.EventType]bool{}

// envChats lists every chat an envelope might act on, and every project path it names. Handlers
// differ on which of the envelope's sessionId and the payload's they use (and on case: JSON
// decoding matches field names loosely), so every candidate is returned and the caller must own
// each one. A chat-less envelope acts on "default".
func envChats(env *protocol.Envelope) (ids, paths []string) {
	add := func(s string) {
		if s != "" && !strings.ContainsRune(s, 0) {
			for _, have := range ids {
				if have == s {
					return
				}
			}
			ids = append(ids, s)
		}
	}
	add(env.SessionID)
	var m map[string]json.RawMessage
	if json.Unmarshal(env.Data, &m) == nil {
		for k, raw := range m {
			var s string
			switch {
			case strings.EqualFold(k, "sessionId"), strings.EqualFold(k, "linkedSessionId"):
				if json.Unmarshal(raw, &s) == nil {
					add(s)
				}
			case strings.EqualFold(k, "projectPath"):
				if json.Unmarshal(raw, &s) == nil && s != "" {
					paths = append(paths, s)
				}
			}
		}
	}
	if len(ids) == 0 && !projectLevelChatEvents[env.Type] {
		ids = []string{"default"}
	}
	return ids, paths
}

// ownsEnvelope reports whether the connection owns every chat env acts on.
func ownsEnvelope(deps connDeps, acc Access, env *protocol.Envelope) bool {
	ids, paths := envChats(env)
	if len(paths) == 0 {
		paths = []string{""}
	}
	for _, id := range ids {
		for _, p := range paths {
			if !ownsChat(deps, acc, id, p) {
				return false
			}
		}
	}
	return true
}

// ensureChat creates sessionID's file in store if it has none, owned by userID (the owner when
// empty), and remembers who owns the chat.
func (r *ProjectRegistry) ensureChat(store agent.SessionStore, sessionID, userID string) error {
	if userID == "" {
		userID = ownerID
	}
	if err := store.EnsureOwned(sessionID, userID); err != nil {
		return err
	}
	if st, err := store.Load(sessionID); err == nil {
		r.rememberOwner(sessionID, stateOwner(st))
	}
	return nil
}

// mayTouchNotifications reports whether acc may dismiss or mark read the notification id: one about
// a chat needs that chat to be visible to them. An id nobody has is left alone either way.
func (d connDeps) mayTouchNotifications(acc Access, id string) bool {
	for _, n := range d.notify.List() {
		if n.ID == id {
			return n.SessionID == "" || d.mayViewChat(acc, n.SessionID)
		}
	}
	return true
}
