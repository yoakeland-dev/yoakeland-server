package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
)

// resolveID follows merges in a copied user map.
func resolveID(users map[string]User, id string) string {
	for range 8 {
		u, ok := users[id]
		if !ok || u.AliasOf == "" {
			return id
		}
		id = u.AliasOf
	}
	return id
}

// manages reports whether acc may manage the person uid: Full may manage anyone, a Trusted person
// only themselves and the people they created.
func manages(acc Access, users map[string]User, uid string) bool {
	if acc.Full() {
		return true
	}
	if !acc.AtLeast(TierTrusted) {
		return false
	}
	uid = resolveID(users, uid)
	u, ok := users[uid]
	return ok && (uid == acc.UserID || (u.CreatedBy != "" && resolveID(users, u.CreatedBy) == acc.UserID))
}

// peopleFor is the people reply as acc may read it.
func (d connDeps) peopleFor(acc Access) protocol.PeopleData {
	users, tokens, limited := d.settings.view()
	cli := d.cliProviderNames()
	sort.Strings(cli)
	online := map[string]bool{}
	for _, p := range d.peers.All() {
		if p.TokenName != "" {
			online[p.TokenName] = true
		}
	}
	out := protocol.PeopleData{
		Users: []protocol.PeoplePerson{}, LimitedDefaults: limited, CLIProviders: cli,
		TierDefaults: map[string]map[string]any{
			TierLimited: computeCaps(TierLimited, nil, nil, nil).ToMap(cli),
			TierTrusted: computeCaps(TierTrusted, nil, nil, nil).ToMap(cli),
			TierFull:    allCaps().ToMap(cli),
		},
	}
	if out.LimitedDefaults == nil {
		out.LimitedDefaults = map[string]any{}
	}
	for id, u := range users {
		if u.AliasOf != "" || !manages(acc, users, id) {
			continue
		}
		p := protocol.PeoplePerson{
			ID: id, Name: u.Name, Avatar: u.Avatar, Tier: u.Tier, Caps: u.Caps, CreatedBy: u.CreatedBy,
			EffectiveCaps: computeCaps(u.Tier, u.Caps, limited, nil).ToMap(cli), Devices: []protocol.PeopleDevice{},
		}
		for cred, rec := range tokens {
			if resolveID(users, rec.UserID) != id {
				continue
			}
			deny := rec.Deny
			if deny == nil {
				deny = []string{}
			}
			p.Devices = append(p.Devices, protocol.PeopleDevice{
				Credential: cred, DeviceType: rec.DeviceType, DeviceName: rec.DeviceName, MaxTier: rec.MaxTier,
				Deny: deny, LastSeen: rec.LastSeen, PairedBy: rec.PairedBy, Online: online[cred],
			})
		}
		sort.Slice(p.Devices, func(i, j int) bool { return p.Devices[i].Credential < p.Devices[j].Credential })
		out.Users = append(out.Users, p)
	}
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].ID < out.Users[j].ID })
	return out
}

// broadcastPeople tells every connection that may read it the current people.
func (d connDeps) broadcastPeople() {
	for _, p := range d.peers.All() {
		acc := d.accessFor(p.Network, p.ID)
		if acc.AtLeast(TierTrusted) {
			_ = p.ipc.SendEvent(protocol.EventPeople, "", "", "", d.peopleFor(acc))
		}
	}
}

// auditUser records an access change in the usual style: who did it, to what, and what changed.
func (d connDeps) auditUser(ipc *protocol.IPCStream, peerID, target string, detail map[string]any) {
	d.record(ipc, peerID, audit.Event{Kind: audit.UserChange, Target: target, Outcome: "ok", Detail: detail})
}

// handlePeopleEnvelope answers the events about people, devices and pairing; it reports whether
// env was one. The event table has already refused anyone below the tier an event needs; what is
// left here is what a table row can't say: a Trusted sender may act only on people they created.
func handlePeopleEnvelope(ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope) bool {
	switch env.Type {
	case protocol.EventListPeople, protocol.EventSetUserPermissions, protocol.EventSetDeviceRestrict, protocol.EventSetLimitedDefaults,
		protocol.EventMergeUsers, protocol.EventMoveDevice, protocol.EventRemoveUser,
		protocol.EventRegisterToken, protocol.EventListTokens, protocol.EventRemoveToken, protocol.EventApprovePairing:
	default:
		return false
	}
	fail := func(format string, args ...any) {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: fmt.Sprintf(format, args...)})
	}
	bad := func() { fail("malformed %s payload", env.Type) }
	acc := deps.accessFor(network, peerID)
	// Changes that can lower access cancel the affected turns and re-sync live connections.
	before := deps.accessSnapshot()
	changed := func(target string, detail map[string]any) {
		deps.auditUser(ipc, peerID, target, detail)
		deps.enforceAccessChange(before)
		deps.broadcastPeople()
	}

	switch env.Type {
	case protocol.EventListPeople:
		_ = ipc.SendEvent(protocol.EventPeople, env.SessionID, env.TurnID, "", deps.peopleFor(acc))

	case protocol.EventSetUserPermissions:
		var d protocol.SetUserPermissionsData
		if json.Unmarshal(env.Data, &d) != nil || d.UserID == "" {
			bad()
			return true
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(env.Data, &raw)
		_, setCaps := raw["caps"]
		if err := deps.settings.SetUserPermissions(d.UserID, d.Tier, d.Caps, setCaps); err != nil {
			fail("%v", err)
			return true
		}
		detail := map[string]any{}
		if d.Tier != nil {
			detail["tier"] = *d.Tier
		}
		if setCaps {
			detail["caps"] = d.Caps
		}
		changed(d.UserID, detail)

	case protocol.EventSetDeviceRestrict:
		var d protocol.SetDeviceRestrictionsData
		if json.Unmarshal(env.Data, &d) != nil || d.Credential == "" {
			bad()
			return true
		}
		if err := deps.settings.SetDeviceRestrictions(d.Credential, d.MaxTier, d.Deny); err != nil {
			fail("%v", err)
			return true
		}
		detail := map[string]any{}
		if d.MaxTier != nil {
			detail["maxTier"] = *d.MaxTier
		}
		if d.Deny != nil {
			detail["deny"] = *d.Deny
		}
		changed(d.Credential, detail)

	case protocol.EventSetLimitedDefaults:
		var d protocol.SetLimitedDefaultsData
		if json.Unmarshal(env.Data, &d) != nil {
			bad()
			return true
		}
		if err := deps.settings.SetLimitedDefaults(d.Caps); err != nil {
			fail("%v", err)
			return true
		}
		changed("limited_defaults", map[string]any{"caps": d.Caps})

	case protocol.EventMergeUsers:
		var d protocol.MergeUsersData
		if json.Unmarshal(env.Data, &d) != nil || d.From == "" || d.Into == "" {
			bad()
			return true
		}
		if err := deps.settings.MergeUsers(d.From, d.Into); err != nil {
			fail("%v", err)
			return true
		}
		changed(d.From, map[string]any{"mergedInto": d.Into})

	case protocol.EventMoveDevice:
		var d protocol.MoveDeviceData
		if json.Unmarshal(env.Data, &d) != nil || d.Credential == "" || d.UserID == "" {
			bad()
			return true
		}
		if err := deps.settings.MoveDevice(d.Credential, d.UserID); err != nil {
			fail("%v", err)
			return true
		}
		changed(d.Credential, map[string]any{"movedTo": d.UserID})

	case protocol.EventRemoveUser:
		var d protocol.RemoveUserData
		if json.Unmarshal(env.Data, &d) != nil || d.UserID == "" {
			bad()
			return true
		}
		revoked, err := deps.settings.RemoveUser(d.UserID)
		if err != nil {
			fail("%v", err)
			return true
		}
		dropped := 0
		for _, cred := range revoked {
			dropped += deps.peers.DisconnectByToken(cred)
		}
		changed(d.UserID, map[string]any{"removed": true, "revoked": len(revoked), "disconnected": dropped})

	case protocol.EventRegisterToken:
		handleRegisterToken(ipc, deps, network, peerID, env, acc)

	case protocol.EventListTokens:
		users, tokens, _ := deps.settings.view()
		out := []protocol.TokenSummary{}
		for _, s := range deps.settings.ListTokenNames() {
			rec := tokens[s.Name]
			if !manages(acc, users, rec.UserID) {
				continue
			}
			out = append(out, protocol.TokenSummary{Name: s.Name, Email: s.Email, HasAvatar: s.HasAvatar, Restricted: s.Restricted,
				UserID: resolveID(users, rec.UserID), Tier: deps.settings.Access("tcp", s.Name).Tier})
		}
		_ = ipc.SendEvent(protocol.EventTokenList, env.SessionID, env.TurnID, "", protocol.TokenListData{Tokens: out})

	case protocol.EventRemoveToken:
		var d protocol.RemoveTokenData
		if json.Unmarshal(env.Data, &d) != nil || d.Name == "" {
			fail("malformed remove_token payload (name is required)")
			return true
		}
		if rec, ok := deps.settings.TokenRecordOf(d.Name); ok {
			users, _, _ := deps.settings.view()
			if !manages(acc, users, rec.UserID) {
				fail("remove_token isn't allowed for this person: that device belongs to someone they didn't add")
				return true
			}
		}
		if _, err := deps.settings.RemoveToken(d.Name); err != nil {
			fail("failed to remove token: %v", err)
			return true
		}
		// Revocation ends the session: whoever is connected on the credential is disconnected now,
		// and the turns it started stop.
		dropped := deps.peers.DisconnectByToken(d.Name)
		deps.record(ipc, peerID, audit.Event{Kind: audit.TokenRemove, Target: d.Name, Outcome: "ok", Detail: map[string]any{"disconnected": dropped}})
		deps.enforceAccessChange(before)
		deps.broadcastPeople()
		_ = ipc.SendEvent(protocol.EventTokenRemoved, env.SessionID, env.TurnID, "", protocol.TokenRemovedData{Name: d.Name})

	case protocol.EventApprovePairing:
		var d protocol.ApprovePairingData
		if json.Unmarshal(env.Data, &d) != nil || d.Credential == "" {
			fail("malformed approve_pairing payload (credential is required)")
			return true
		}
		// A Trusted sender answers only the pairings they made, and can't hand out Full.
		if !acc.Full() {
			rec, ok := deps.settings.TokenRecordOf(d.Credential)
			users, _, _ := deps.settings.view()
			if !ok || rec.PairedBy == "" || resolveID(users, rec.PairedBy) != acc.UserID {
				fail("approve_pairing isn't allowed for this person: they didn't create that pairing")
				return true
			}
			if d.Allow && d.Full {
				fail("approving with full access needs Full access")
				return true
			}
		}
		if d.Allow && d.Full {
			if deps.settings.SetTokenFull(d.Credential) {
				deps.auditUser(ipc, peerID, d.Credential, map[string]any{"pairingApprovedFull": true})
			}
			deps.syncPeerAccess()
			deps.broadcastPeople()
		}
		deps.pairings.answer(d.Credential, d.Allow)
	}
	return true
}

// handleRegisterToken mints a credential, for the sender or for someone else, and answers with its
// token. Nobody can grant more than their own effective tier.
func handleRegisterToken(ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope, acc Access) {
	fail := func(format string, args ...any) {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: fmt.Sprintf(format, args...)})
	}
	var d protocol.RegisterTokenData
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Name == "" {
		fail("malformed register_token payload (name is required)")
		return
	}
	mode := d.User
	if mode == "" {
		mode = "new" // an old client's pairing code, or a plain token
	}
	if d.Tier != "" && !validTier(d.Tier) || d.MaxTier != "" && !validTier(d.MaxTier) {
		fail("tier must be limited, trusted or full")
		return
	}
	maxGrant := TierLimited // what this sender may give a new person
	if acc.Full() {
		maxGrant = TierFull
	}
	c := NewCredential{Name: d.Name, Token: "", Email: d.Email, Pairing: d.Pairing, By: acc.UserID, MaxTier: d.MaxTier}
	switch mode {
	case "me":
		if !acc.Full() {
			fail("register_token for your own devices needs Full access")
			return
		}
		c.UserID = acc.UserID
		// A device paired as "me" is limited unless the sender asks otherwise.
		if d.MaxTier == "" && !d.Full {
			c.MaxTier = TierLimited
		}
	case "new":
		tier := d.Tier
		if tier == "" {
			switch {
			case d.Full:
				tier = TierFull
			case d.Pairing, !acc.Full():
				tier = TierLimited
			default:
				tier = TierFull // a plain token, as before
			}
		}
		if tierRank(tier) > tierRank(maxGrant) {
			fail("register_token can't give %s access: that is above your own", tier)
			return
		}
		c.NewUserName, c.NewUserTier = d.NewUserName, tier
		if c.NewUserName != "" {
			c.NewUserName = cleanName(c.NewUserName)
		}
	default:
		// Only the computer running the server can add a device to someone already known.
		if !acc.Local {
			fail("register_token for an existing person needs the computer running the server")
			return
		}
		u, ok := deps.settings.ResolveUser(mode)
		if !ok {
			fail("no such person %q", mode)
			return
		}
		c.UserID = u.ID
	}
	if c.MaxTier == TierFull {
		c.MaxTier = "" // no ceiling
	}
	token, err := generateAuthToken()
	if err != nil {
		fail("failed to generate token: %v", err)
		return
	}
	c.Token = token
	userID, err := deps.settings.AddCredential(c)
	if err != nil {
		fail("%v", err)
		return
	}
	u, _ := deps.settings.ResolveUser(userID)
	deps.record(ipc, peerID, audit.Event{Kind: audit.TokenAdd, Target: d.Name, Outcome: "ok", Detail: map[string]any{
		"user": userID, "tier": u.Tier, "maxTier": c.MaxTier, "pairing": d.Pairing,
	}}) // the name, never the token
	deps.broadcastPeople()
	// Best-effort: a client can still pair by pasting the CA separately (or doesn't need one at
	// all, on an operator-supplied cert) if this fails.
	caPem, _ := deps.remote.CAPEM()
	_ = ipc.SendEvent(protocol.EventTokenRegistered, env.SessionID, env.TurnID, "", protocol.TokenRegisteredData{
		Name: d.Name, UserID: userID, Token: token, Email: d.Email, CaPem: caPem,
		RTCFingerprint: deps.remote.RTCFingerprint(),
	})
}
