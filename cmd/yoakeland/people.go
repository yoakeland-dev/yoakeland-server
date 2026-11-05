package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

func tierRank(t string) int {
	switch t {
	case TierFull:
		return 2
	case TierTrusted:
		return 1
	}
	return 0 // limited, and anything unrecognised
}

func validTier(t string) bool { return t == TierLimited || t == TierTrusted || t == TierFull }

// minTier is the lower of a person's tier and a device ceiling (empty means no ceiling).
func minTier(userTier, ceiling string) string {
	if ceiling == "" {
		ceiling = TierFull
	}
	if tierRank(ceiling) < tierRank(userTier) {
		return ceiling
	}
	if !validTier(userTier) {
		return TierLimited
	}
	return userTier
}

// resolveUserLocked follows AliasOf from id to the person it stands for. A broken or looping
// chain resolves to nothing.
func (s *ServerSettings) resolveUserLocked(id string) (User, bool) {
	for range 8 {
		u, ok := s.Users[id]
		if !ok {
			return User{}, false
		}
		if u.AliasOf == "" {
			return u, true
		}
		id = u.AliasOf
	}
	return User{}, false
}

// ResolveUser is the person id stands for, following merges.
func (s *ServerSettings) ResolveUser(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveUserLocked(id)
}

// GetUser returns a copy of the user stored under id, aliases included, unresolved.
func (s *ServerSettings) GetUser(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Users[id]
	return cloneUser(u), ok
}

func cloneUser(u User) User {
	u.Caps = maps.Clone(u.Caps)
	u.Names = slices.Clone(u.Names)
	return u
}

// SeedOwnerProfile fills the owner's empty name and picture from the first local-socket device
// that reports them; whatever is already there is never overwritten this way.
func (s *ServerSettings) SeedOwnerProfile(name, avatar string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.Users[ownerID]
	changed := false
	if u.Name == "" && name != "" {
		u.Name, changed = name, true
	}
	if u.Avatar == "" && avatar != "" && len(avatar) <= maxAvatarLen {
		u.Avatar, changed = avatar, true
	}
	if changed {
		s.Users[ownerID] = u
		_ = s.save()
	}
}

// TouchToken records that the credential was seen now.
func (s *ServerSettings) TouchToken(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.Tokens[name]; ok {
		rec.LastSeen = time.Now().Unix()
		s.Tokens[name] = rec
		_ = s.save()
	}
}

func (s *ServerSettings) userNameLocked(id string) string {
	if u, ok := s.resolveUserLocked(id); ok {
		if u.Name != "" {
			return u.Name
		}
		return u.ID
	}
	return ""
}

// UserName is the display name of id's person ("" if unknown).
func (s *ServerSettings) UserName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userNameLocked(id)
}

func (s *ServerSettings) errUnknownUser(id string) error { return fmt.Errorf("no such person %q", id) }

func cleanUserName(n string) (string, error) {
	n = strings.TrimSpace(cleanName(n))
	if n == "" {
		return "", fmt.Errorf("a name is required")
	}
	return n, nil
}

// SetProviderOwner records who registered the provider name.
func (s *ServerSettings) SetProviderOwner(name, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ProviderOwners == nil {
		s.ProviderOwners = map[string]string{}
	}
	s.ProviderOwners[name] = userID
	_ = s.save()
}

// ProviderOwner is who registered the provider name ("" if a Full person or the owner did).
func (s *ServerSettings) ProviderOwner(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ProviderOwners[name]
}

// credentialLive reports whether the credential exists and its person still does.
func (s *ServerSettings) credentialLive(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	if !ok {
		return false
	}
	_, ok = s.resolveUserLocked(rec.UserID)
	return ok
}

// AccessSnapshot is every credential's access now.
func (s *ServerSettings) AccessSnapshot() map[string]Access {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Access, len(s.Tokens))
	for name := range s.Tokens {
		out[name] = s.accessLocked(name)
	}
	return out
}
