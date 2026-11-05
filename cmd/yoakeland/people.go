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

// TouchToken records that the credential was seen now, and what its device calls itself.
func (s *ServerSettings) TouchToken(name, deviceType, deviceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.Tokens[name]; ok {
		rec.LastSeen = time.Now().Unix()
		if deviceType != "" {
			rec.DeviceType = deviceType
		}
		if deviceName != "" {
			rec.DeviceName = deviceName
		}
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

// TokenRecordOf returns a copy of the credential's record.
func (s *ServerSettings) TokenRecordOf(name string) (TokenRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	rec.Deny = slices.Clone(rec.Deny)
	return rec, ok
}

// CreateUser adds a person.
func (s *ServerSettings) CreateUser(name, email, tier, createdBy string) (User, error) {
	name, err := cleanUserName(name)
	if err != nil {
		return User{}, err
	}
	if !validTier(tier) {
		return User{}, fmt.Errorf("tier must be limited, trusted or full")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.newUserLocked(name, email, "", tier, createdBy)
	return u, s.save()
}

// SetUserPermissions changes a person's tier and/or capability overlay. The owner can't be changed.
func (s *ServerSettings) SetUserPermissions(id string, tier *string, caps map[string]any, setCaps bool) error {
	if id == ownerID {
		return fmt.Errorf("the owner's access can't be changed")
	}
	if tier != nil && !validTier(*tier) {
		return fmt.Errorf("tier must be limited, trusted or full")
	}
	if setCaps {
		if err := validateCaps(caps); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Users[id]
	if !ok || u.AliasOf != "" {
		return s.errUnknownUser(id)
	}
	if tier != nil {
		u.Tier = *tier
	}
	if setCaps {
		u.Caps = maps.Clone(caps)
		if len(u.Caps) == 0 {
			u.Caps = nil
		}
	}
	s.Users[id] = u
	return s.save()
}

// SetDeviceRestrictions changes a credential's ceiling and/or the capabilities denied on it.
func (s *ServerSettings) SetDeviceRestrictions(cred string, maxTier *string, deny *[]string) error {
	if maxTier != nil && *maxTier != "" && !validTier(*maxTier) {
		return fmt.Errorf("maxTier must be limited, trusted, full or empty")
	}
	if deny != nil {
		for _, d := range *deny {
			if !slices.Contains(capNames, d) {
				return fmt.Errorf("unknown capability %q", d)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[cred]
	if !ok {
		return fmt.Errorf("no such device %q", cred)
	}
	if maxTier != nil {
		rec.MaxTier = *maxTier
	}
	if deny != nil {
		rec.Deny = slices.Clone(*deny)
		if len(rec.Deny) == 0 {
			rec.Deny = nil
		}
	}
	s.Tokens[cred] = rec
	return s.save()
}

// SetLimitedDefaults replaces what Limited people have beyond the built-in defaults.
func (s *ServerSettings) SetLimitedDefaults(caps map[string]any) error {
	if err := validateCaps(caps); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LimitedDefaults = maps.Clone(caps)
	if len(s.LimitedDefaults) == 0 {
		s.LimitedDefaults = nil
	}
	return s.save()
}

// MergeUsers makes from an alias of into: its devices move to into, and its names are kept on
// into so old messages still match. The owner can't be merged away.
func (s *ServerSettings) MergeUsers(from, into string) error {
	if from == ownerID {
		return fmt.Errorf("the owner can't be merged into someone else")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.Users[from]
	if !ok || f.AliasOf != "" {
		return s.errUnknownUser(from)
	}
	t, ok := s.resolveUserLocked(into)
	if !ok {
		return s.errUnknownUser(into)
	}
	if t.ID == f.ID {
		return fmt.Errorf("can't merge a person into themselves")
	}
	for _, n := range append([]string{f.Name}, f.Names...) {
		if n != "" && n != t.Name && !slices.Contains(t.Names, n) {
			t.Names = append(t.Names, n)
		}
	}
	s.Users[t.ID] = t
	f.AliasOf = t.ID
	s.Users[from] = f
	for name, rec := range s.Tokens {
		if rec.UserID == from {
			rec.UserID = t.ID
			s.Tokens[name] = rec
		}
	}
	// Anyone already merged into from now stands for t.
	for id, u := range s.Users {
		if u.AliasOf == from {
			u.AliasOf = t.ID
			s.Users[id] = u
		}
	}
	return s.save()
}

// MoveDevice points a credential at another person.
func (s *ServerSettings) MoveDevice(cred, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[cred]
	if !ok {
		return fmt.Errorf("no such device %q", cred)
	}
	u, ok := s.resolveUserLocked(userID)
	if !ok {
		return s.errUnknownUser(userID)
	}
	rec.UserID = u.ID
	s.Tokens[cred] = rec
	return s.save()
}

// RemoveUser deletes a person, everyone merged into them, and all their credentials. It returns
// the credentials it revoked. The owner can't be removed. Their messages and chats stay with the
// chats: nothing here touches them.
func (s *ServerSettings) RemoveUser(id string) (revoked []string, err error) {
	if id == ownerID {
		return nil, fmt.Errorf("the owner can't be removed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Users[id]; !ok {
		return nil, s.errUnknownUser(id)
	}
	gone := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for uid, u := range s.Users {
			if !gone[uid] && gone[u.AliasOf] {
				gone[uid], changed = true, true
			}
		}
	}
	for name, rec := range s.Tokens {
		if gone[rec.UserID] {
			delete(s.Tokens, name)
			revoked = append(revoked, name)
		}
	}
	for uid := range gone {
		delete(s.Users, uid)
	}
	slices.Sort(revoked)
	return revoked, s.save()
}

// SetTokenFull is approve_pairing's "full": the credential's person becomes Full and the device
// ceiling is lifted. False if no such credential.
func (s *ServerSettings) SetTokenFull(name string) bool { return s.SetTokenScope(name, true) }

// view returns copies of the people, credentials and Limited defaults, for building a reply.
func (s *ServerSettings) view() (users map[string]User, tokens map[string]TokenRecord, limited map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users = make(map[string]User, len(s.Users))
	for id, u := range s.Users {
		users[id] = cloneUser(u)
	}
	tokens = make(map[string]TokenRecord, len(s.Tokens))
	for n, r := range s.Tokens {
		r.Deny = slices.Clone(r.Deny)
		tokens[n] = r
	}
	return users, tokens, maps.Clone(s.LimitedDefaults)
}
