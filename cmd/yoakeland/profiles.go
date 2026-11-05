package main

import "yoakeland/internal/protocol"

// authorIndex finds who wrote a stored message.
type authorIndex struct {
	users map[string]User
	// byName maps a name a person has or had to the person (resolved through merges). A name two
	// people have or had maps to "": it says nothing about who wrote the message.
	byName map[string]string
}

func newAuthorIndex(users map[string]User) *authorIndex {
	ix := &authorIndex{users: users, byName: map[string]string{}}
	add := func(name, id string) {
		if name == "" {
			return
		}
		if cur, seen := ix.byName[name]; seen && cur != id {
			id = ""
		}
		ix.byName[name] = id
	}
	for id, u := range users {
		to := resolveID(users, id)
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
// author name is exactly a name some person has or had. The stored files are not touched. A
// message whose author can't be placed keeps its stored name and picture.
func (ix *authorIndex) apply(msgs []protocol.HistoryMessage) map[string]protocol.ProfileInfo {
	var profiles map[string]protocol.ProfileInfo
	for i := range msgs {
		m := &msgs[i]
		id := m.AuthorID
		if id == "" && m.Author != "" {
			id = ix.byName[m.Author]
		}
		if id == "" {
			continue
		}
		if profiles == nil {
			profiles = map[string]protocol.ProfileInfo{}
		}
		if u, ok := ix.users[resolveID(ix.users, id)]; ok && u.AliasOf == "" {
			m.AuthorID = u.ID
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
