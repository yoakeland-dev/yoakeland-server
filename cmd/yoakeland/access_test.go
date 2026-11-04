package main

import (
	"reflect"
	"testing"
)

// settingsWith is settings holding the given people and credentials, nothing else.
func settingsWith(users []User, tokens map[string]TokenRecord, limited map[string]any) *ServerSettings {
	s := &ServerSettings{Users: map[string]User{}, Tokens: tokens, LimitedDefaults: limited}
	for _, u := range users {
		s.Users[u.ID] = u
	}
	return s
}

func TestAccess_Resolver(t *testing.T) {
	users := []User{
		{ID: ownerID, Tier: TierFull},
		{ID: "lim", Tier: TierLimited},
		{ID: "tru", Tier: TierTrusted},
		{ID: "ful", Tier: TierFull},
		{ID: "odd", Tier: TierLimited, Caps: map[string]any{CapMCPTools: true, CapBrowser: "watch", CapCLI: []any{"cli_claude"}}},
		{ID: "bad", Tier: TierLimited, Caps: map[string]any{CapFilesEdit: "yes", CapBrowser: "sideways", CapCLI: 7}},
		{ID: "old", Tier: TierLimited, AliasOf: "tru"},
		{ID: "loopA", Tier: TierFull, AliasOf: "loopB"},
		{ID: "loopB", Tier: TierFull, AliasOf: "loopA"},
		{ID: "orphan", Tier: TierFull, AliasOf: "nobody"},
		{ID: "junk", Tier: "wizard"},
	}
	tokens := map[string]TokenRecord{
		"lim":        {UserID: "lim"},
		"tru":        {UserID: "tru"},
		"ful":        {UserID: "ful"},
		"ful-lim":    {UserID: "ful", MaxTier: TierLimited},
		"ful-tru":    {UserID: "ful", MaxTier: TierTrusted},
		"lim-full":   {UserID: "lim", MaxTier: TierFull}, // a ceiling never raises
		"tru-nocmds": {UserID: "tru", Deny: []string{CapCommandsSandboxed, CapBrowser, CapCLI, "no_such_cap"}},
		"odd":        {UserID: "odd"},
		"odd-deny":   {UserID: "odd", Deny: []string{CapMCPTools, CapBrowser, CapCLI}},
		"bad":        {UserID: "bad"},
		"alias":      {UserID: "old"},
		"loop":       {UserID: "loopA"},
		"orphan":     {UserID: "orphan"},
		"nouser":     {UserID: "ghost"},
		"junk":       {UserID: "junk"},
	}
	limitedDefaults := map[string]any{CapProcesses: false, CapTerminalsWatch: true}
	s := settingsWith(users, tokens, limitedDefaults)

	defaults := Caps{FilesEdit: true, CommandsSandboxed: true, Autonomous: true, Processes: true, Browser: browserOff}
	limitedWithDefaults := defaults
	limitedWithDefaults.Processes, limitedWithDefaults.TerminalsWatch = false, true
	none := Caps{Browser: browserOff}

	cases := []struct {
		name, network, cred string
		tier                string
		caps                Caps
		user                string
	}{
		{"local socket", "unix", "", TierFull, allCaps(), ownerID},
		{"stdio", "stdio", "", TierFull, allCaps(), ownerID},
		{"local ignores credentials", "unix", "lim", TierFull, allCaps(), ownerID},
		{"limited gets LimitedDefaults", "tcp", "lim", TierLimited, limitedWithDefaults, "lim"},
		{"trusted does not get LimitedDefaults", "tcp", "tru", TierTrusted, defaults, "tru"},
		{"full is everything", "tcp", "ful", TierFull, allCaps(), "ful"},
		{"min(full, limited ceiling)", "tcp", "ful-lim", TierLimited, limitedWithDefaults, "ful"},
		{"min(full, trusted ceiling)", "tcp", "ful-tru", TierTrusted, defaults, "ful"},
		{"a ceiling above the person changes nothing", "tcp", "lim-full", TierLimited, limitedWithDefaults, "lim"},
		{"deny removes caps, unknown names ignored", "tcp", "tru-nocmds", TierTrusted,
			Caps{FilesEdit: true, Autonomous: true, Processes: true, Browser: browserOff}, "tru"},
		{"user caps overlay the defaults", "tcp", "odd", TierLimited,
			Caps{FilesEdit: true, CommandsSandboxed: true, Autonomous: true, Processes: false, TerminalsWatch: true, MCPTools: true, Browser: browserWatch, CLI: []string{"cli_claude"}}, "odd"},
		{"device deny beats a user grant", "tcp", "odd-deny", TierLimited,
			Caps{FilesEdit: true, CommandsSandboxed: true, Autonomous: true, TerminalsWatch: true, Browser: browserOff, CLI: []string{}}, "odd"},
		{"malformed overlay values are off", "tcp", "bad", TierLimited,
			Caps{CommandsSandboxed: true, Autonomous: true, TerminalsWatch: true, Browser: browserOff}, "bad"},
		{"alias resolves to the merged person", "tcp", "alias", TierTrusted, defaults, "tru"},
		{"alias loop denies", "tcp", "loop", TierLimited, none, ""},
		{"alias to nobody denies", "tcp", "orphan", TierLimited, none, ""},
		{"unknown user denies everything", "tcp", "nouser", TierLimited, none, ""},
		{"unknown credential denies everything", "tcp", "nobody", TierLimited, none, ""},
		{"no credential on tcp denies everything", "tcp", "", TierLimited, none, ""},
		{"a made-up tier is limited", "tcp", "junk", TierLimited, limitedWithDefaults, "junk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Access(tc.network, tc.cred)
			// A nil and an empty CLI list are the same thing.
			if len(got.Caps.CLI) == 0 {
				got.Caps.CLI = nil
			}
			if len(tc.caps.CLI) == 0 {
				tc.caps.CLI = nil
			}
			if got.Tier != tc.tier || got.UserID != tc.user || !reflect.DeepEqual(got.Caps, tc.caps) {
				t.Fatalf("got tier=%s user=%q caps=%+v\nwant tier=%s user=%q caps=%+v", got.Tier, got.UserID, got.Caps, tc.tier, tc.user, tc.caps)
			}
		})
	}
}

func TestAccess_ChangesApplyAtOnce(t *testing.T) {
	s := settingsWith([]User{{ID: "p", Tier: TierTrusted}}, map[string]TokenRecord{"c": {UserID: "p"}}, nil)
	if s.Access("tcp", "c").Tier != TierTrusted {
		t.Fatal("setup")
	}
	u := s.Users["p"]
	u.Tier = TierLimited
	s.Users["p"] = u
	if got := s.Access("tcp", "c").Tier; got != TierLimited {
		t.Fatalf("after lowering the person: %s", got)
	}
	delete(s.Tokens, "c")
	if got := s.Access("tcp", "c"); got.Tier != TierLimited || got.Caps.FilesEdit {
		t.Fatalf("after revoking the credential: %+v", got)
	}
}

func TestCaps_AllowCLIAndValidate(t *testing.T) {
	if c := (Caps{CLI: []string{"a"}}); !c.AllowCLI("a") || c.AllowCLI("b") {
		t.Fatal("listed provider check")
	}
	if !allCaps().AllowCLI("anything") {
		t.Fatal("full allows every CLI agent")
	}
	for _, bad := range []map[string]any{{"nope": true}, {CapFilesEdit: 1}, {CapBrowser: "x"}, {CapCLI: "claude"}, {CapCLI: []any{1}}} {
		if validateCaps(bad) == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
	if err := validateCaps(map[string]any{CapFilesEdit: false, CapBrowser: "use", CapCLI: []any{"x"}}); err != nil {
		t.Fatal(err)
	}
}
