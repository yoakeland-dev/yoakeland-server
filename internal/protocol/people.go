package protocol

// People, devices and tiers (SPEC.md "Tiers and permissions"). A device credential is the login
// and points at a server-owned person; a person has a tier and capabilities, a device can only
// have less than its person.

const (
	EventListPeople         EventType = "list_people"
	EventPeople             EventType = "people"
	EventSetUserPermissions EventType = "set_user_permissions"
	EventSetDeviceRestrict  EventType = "set_device_restrictions"
	EventSetLimitedDefaults EventType = "set_limited_defaults"
	EventMergeUsers         EventType = "merge_users"
	EventMoveDevice         EventType = "move_device"
	EventRemoveUser         EventType = "remove_user"
	EventSetProfile         EventType = "set_profile"
	EventProfileUpdated     EventType = "profile_updated"
	EventGetAvatar          EventType = "get_avatar"
	EventAvatarData         EventType = "avatar_data"
)

// ProfileInfo is a person as a chat shows them: a name and a picture (an emoji or a data URL).
// ID is set where the profile isn't keyed by it (server_identity, profile_updated).
type ProfileInfo struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// SetProfileData is set_profile: change your own name and picture. Each is applied only when set.
// It edits the person, not the device (set_peer_name and set_peer_avatar do that).
type SetProfileData struct {
	Name   *string `json:"name,omitempty"`
	Avatar *string `json:"avatar,omitempty"`
}

// ProfileUpdatedData is profile_updated, sent to every peer when a person changes their profile.
type ProfileUpdatedData = ProfileInfo

// SetUserPermissionsData is set_user_permissions. Tier and Caps are each applied only when set;
// Caps replaces the person's whole overlay (only what differs from the tier default). The owner
// can't be changed.
type SetUserPermissionsData struct {
	UserID string         `json:"userId"`
	Tier   *string        `json:"tier,omitempty"`
	Caps   map[string]any `json:"caps,omitempty"`
}

// SetDeviceRestrictionsData is set_device_restrictions: a device's ceiling ("" clears it) and the
// capabilities taken away on it. Each is applied only when set.
type SetDeviceRestrictionsData struct {
	Credential string    `json:"credential"`
	MaxTier    *string   `json:"maxTier,omitempty"`
	Deny       *[]string `json:"deny,omitempty"`
}

// SetLimitedDefaultsData is set_limited_defaults: it replaces what Limited people have beyond the
// built-in defaults.
type SetLimitedDefaultsData struct {
	Caps map[string]any `json:"caps"`
}

// MergeUsersData is merge_users: From becomes an alias of Into, its devices move, its names are
// kept so old messages still match. The owner can't be From.
type MergeUsersData struct {
	From string `json:"from"`
	Into string `json:"into"`
}

// MoveDeviceData is move_device.
type MoveDeviceData struct {
	Credential string `json:"credential"`
	UserID     string `json:"userId"`
}

// RemoveUserData is remove_user: revokes the person's devices and disconnects them. Their
// messages and chats stay. The owner can't be removed.
type RemoveUserData struct {
	UserID string `json:"userId"`
}

// PeopleDevice is one credential in a people reply. It never carries the token value.
type PeopleDevice struct {
	Credential string   `json:"credential"`
	DeviceType string   `json:"deviceType,omitempty"`
	DeviceName string   `json:"deviceName,omitempty"`
	MaxTier    string   `json:"maxTier,omitempty"`
	Deny       []string `json:"deny"`
	LastSeen   int64    `json:"lastSeen,omitempty"` // unix seconds
	PairedBy   string   `json:"pairedBy,omitempty"` // user id
	Online     bool     `json:"online"`
}

// PeoplePerson is one person in a people reply. Caps is what differs from the tier default,
// EffectiveCaps what they have.
type PeoplePerson struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Avatar        string         `json:"avatar,omitempty"`
	Names         []string       `json:"names,omitempty"` // earlier names, which old messages still match
	Tier          string         `json:"tier"`
	Caps          map[string]any `json:"caps,omitempty"`
	EffectiveCaps map[string]any `json:"effectiveCaps"`
	CreatedBy     string         `json:"createdBy,omitempty"`
	Devices       []PeopleDevice `json:"devices"`
}

// PeopleData is people, the reply to list_people and the broadcast after any change to people or
// devices. A Trusted reader sees only the people they created, and themselves.
type PeopleData struct {
	Users           []PeoplePerson            `json:"users"`
	LimitedDefaults map[string]any            `json:"limitedDefaults"`
	TierDefaults    map[string]map[string]any `json:"tierDefaults"`
	CLIProviders    []string                  `json:"cliProviders"`
}

// GetAvatarData is get_avatar: fetch the picture an "upload:<sha256>" avatar names.
type GetAvatarData struct {
	Avatar string `json:"avatar"`
}

// AvatarDataData is avatar_data, the reply: the same avatar value, and the picture as a data URL
// (empty when the server has no such picture). A picture never changes under its hash, so a
// client may keep it for good.
type AvatarDataData struct {
	Avatar string `json:"avatar"`
	Data   string `json:"data,omitempty"`
}
