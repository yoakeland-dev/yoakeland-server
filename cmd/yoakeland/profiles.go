package main

import (
	"strings"

	"yoakeland/internal/protocol"
)

// authorIndex finds who wrote a stored message.
type authorIndex struct {
	users map[string]User
	// byName maps a name a person has or had to the person (resolved through merges). A name two
	// people have or had maps to "": it says nothing about who wrote the message.
	byName map[string]string
	// byAvatar maps a picture (see avatarKey) to the person who has it, likewise "" when two do.
	// It places a message whose author name no one has had, such as a device name given to a
	// person at migration, but whose picture is theirs.
	byAvatar map[string]string
}

func newAuthorIndex(users map[string]User) *authorIndex {
	ix := &authorIndex{users: users, byName: map[string]string{}, byAvatar: map[string]string{}}
	add := func(name, id string) {
		if name == "" {
			return
		}
		if cur, seen := ix.byName[name]; seen && cur != id {
			id = ""
		}
		ix.byName[name] = id
	}
	addAvatar := func(a, id string) {
		if a == "" {
			return
		}
		k := avatarKey(a)
		if !sha256Hex.MatchString(k) { // an emoji is shared by anyone, so it places no one
			return
		}
		if cur, seen := ix.byAvatar[k]; seen && cur != id {
			id = ""
		}
		ix.byAvatar[k] = id
	}
	for id, u := range users {
		to := resolveID(users, id)
		addAvatar(u.Avatar, to)
		add(u.Name, to)
		for _, n := range u.Names {
			add(n, to)
		}
	}
	return ix
}

// apply gives each message of msgs its author's id and returns the profiles of the authors in it,
// each once. A message stored with an authorId gets the person it stands for now (a merged person's
// old id becomes the person's current one). One from before authorId existed gets one when its
// author name is exactly a name some person has or had, or else when its picture is theirs.
// A message placed this way drops its own copy of a picture (an emoji stays). The stored files are not touched. A
// message whose author can't be placed keeps its stored name and picture.
func (ix *authorIndex) apply(msgs []protocol.HistoryMessage) map[string]protocol.ProfileInfo {
	var profiles map[string]protocol.ProfileInfo
	for i := range msgs {
		m := &msgs[i]
		id := m.AuthorID
		if id == "" && m.Author != "" {
			id = ix.byName[m.Author]
		}
		if id == "" && m.AuthorAvatar != "" {
			id = ix.byAvatar[avatarKey(m.AuthorAvatar)] // "" for an emoji: none is indexed
		}
		if id == "" {
			continue
		}
		if profiles == nil {
			profiles = map[string]protocol.ProfileInfo{}
		}
		if u, ok := ix.users[resolveID(ix.users, id)]; ok && u.AliasOf == "" {
			m.AuthorID = u.ID
			if strings.HasPrefix(m.AuthorAvatar, "data:") {
				m.AuthorAvatar = "" // a picture rides in the profile, once, not on every message
			}
			profiles[u.ID] = protocol.ProfileInfo{Name: u.Name, Avatar: u.Avatar}
		} else if _, seen := profiles[id]; !seen { // a person who has since been removed
			m.AuthorID = id
			profiles[id] = protocol.ProfileInfo{Name: m.Author, Avatar: m.AuthorAvatar}
		}
	}
	return profiles
}

// withProfiles fills in the authors of a session_history reply.
func (d connDeps) withProfiles(msgs []protocol.HistoryMessage, out protocol.SessionHistoryData) protocol.SessionHistoryData {
	users, _, _ := d.settings.view()
	out.Profiles = newAuthorIndex(users).apply(msgs)
	out.Messages = msgs
	return out
}
