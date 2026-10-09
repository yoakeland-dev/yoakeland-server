package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skill(t *testing.T, root, dir, name, desc string) string {
	t.Helper()
	p := filepath.Join(root, dir)
	write(t, filepath.Join(p, File), "---\nname: "+name+"\ndescription: "+desc+"\n---\n# "+name+"\nDo the thing.\n")
	return p
}

func find(skills []Skill, name string) *Skill {
	for i := range skills {
		if skills[i].Name == name {
			return &skills[i]
		}
	}
	return nil
}

func TestFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		check     func(frontmatter) bool
	}{
		{"plain", "---\nname: a\ndescription: Does a thing\n---\nBody", func(f frontmatter) bool { return f.name == "a" && f.description == "Does a thing" && f.body == "Body" }},
		{"quoted", "---\nname: \"a:b\"\ndescription: 'It''s fine'\n---\nB", func(f frontmatter) bool { return f.name == "a:b" && f.description == "It's fine" }},
		{"folded", "---\nname: a\ndescription: >-\n  Review the code\n  and flag risks.\nlicense: MIT\n---\nB", func(f frontmatter) bool {
			return f.description == "Review the code and flag risks." && f.license == "MIT"
		}},
		{"literal", "---\ndescription: |\n  line one\n  line two\n---\nB", func(f frontmatter) bool { return f.description == "line one\nline two" }},
		{"nested map is skipped", "---\nname: a\nmetadata:\n  author: x\n  version: \"1\"\ndescription: d\n---\nB", func(f frontmatter) bool { return f.name == "a" && f.description == "d" }},
		{"yaml 1.1 booleans", "---\nname: a\nuser-invocable: no\ndisable-model-invocation: yes\n---\nB", func(f frontmatter) bool { return !f.userInvocable && f.disableModel }},
		{"defaults", "---\nname: a\n---\nB", func(f frontmatter) bool { return f.userInvocable && !f.disableModel }},
		{"inline comment", "---\nname: a # the name\n---\nB", func(f frontmatter) bool { return f.name == "a" }},
		{"crlf", "---\r\nname: a\r\ndescription: d\r\n---\r\nBody\r\n", func(f frontmatter) bool {
			return f.name == "a" && f.description == "d" && strings.TrimSpace(f.body) == "Body"
		}},
		{"no frontmatter", "# Just a doc\nText", func(f frontmatter) bool { return f.name == "" && f.body == "# Just a doc\nText" }},
		{"unclosed", "---\nname: a\nno end", func(f frontmatter) bool { return f.malformed }},
		{"bom", "\ufeff---\nname: a\n---\nB", func(f frontmatter) bool { return f.name == "a" }},
		{"dashes in body are not the end of an unfenced doc", "text\n---\nname: no\n---\n", func(f frontmatter) bool { return f.name == "" }},
	} {
		if f := parseFrontmatter(tc.doc); !tc.check(f) {
			t.Errorf("%s: parsed %+v", tc.name, f)
		}
	}
}

func TestScanFindsSkillsAcrossRootsAndFoldsDuplicates(t *testing.T) {
	home, proj, state := t.TempDir(), t.TempDir(), t.TempDir()
	// The same folder reached through two roots (a symlink farm), a skill only
	// Claude reads, a project skill shadowing a user one, and non-skills.
	shared := skill(t, filepath.Join(home, ".agents", "skills"), "pdf", "pdf", "Read PDFs")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(home, ".claude", "skills", "pdf")); err != nil {
		t.Skip("no symlinks here")
	}
	skill(t, filepath.Join(home, ".claude", "skills"), "only-claude", "only-claude", "Claude only")
	skill(t, filepath.Join(home, ".agents", "skills"), "review", "review", "User review")
	skill(t, filepath.Join(proj, ".agents", "skills"), "review", "review", "Project review")
	skill(t, filepath.Join(state, "skills"), "mine", "mine", "Yoake's own")
	write(t, filepath.Join(home, ".agents", "skills", "not-a-skill", "README.md"), "x")
	write(t, filepath.Join(home, ".agents", "skills", ".hidden", File), "---\nname: h\n---\n")
	write(t, filepath.Join(home, ".agents", "skills", "loose-file.md"), "x")

	sk, problems := Scan(Roots(state, home, proj), nil)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	names := map[string]int{}
	for _, s := range sk {
		names[s.Name]++
	}
	if names["pdf"] != 1 {
		t.Fatalf("the same folder through two roots is one skill, got %d", names["pdf"])
	}
	pdf := find(sk, "pdf")
	if len(pdf.AlsoIn) != 1 || pdf.AlsoIn[0] != "claude" || !pdf.For("codex") || !pdf.For("claude") || !pdf.For(Native) {
		t.Fatalf("pdf = %+v (it should serve every agent either root serves)", pdf)
	}
	if find(sk, "not-a-skill") != nil || find(sk, "h") != nil || len(sk) != 5 {
		t.Fatalf("found %d skills: %+v", len(sk), sk)
	}
	oc := find(sk, "only-claude")
	if !oc.For("claude") || oc.For("codex") {
		t.Fatalf("only-claude agents = %v", oc.Agents)
	}
	if !find(sk, "mine").For(Native) || find(sk, "mine").For("claude") {
		t.Fatal("a skill in yoakeland's own folder is for yoakeland's own loop")
	}
	// Both reviews exist; the project's wins.
	var proj1, user1 *Skill
	for i := range sk {
		if sk[i].Name == "review" {
			if sk[i].Scope == ScopeProject {
				proj1 = &sk[i]
			} else {
				user1 = &sk[i]
			}
		}
	}
	if proj1 == nil || user1 == nil || proj1.Shadowed || !user1.Shadowed || user1.Active() {
		t.Fatalf("project review = %+v, user review = %+v", proj1, user1)
	}
	if got, ok := Find(sk, "review", Native); !ok || got.Description != "Project review" {
		t.Fatalf("Find(review) = %+v", got)
	}
	if sk[0].Scope != ScopeProject {
		t.Fatal("project skills list first")
	}
}

func TestScanTolerance(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "no-desc", File), "---\nname: no-desc\n---\nThe first paragraph explains it.\n\nSecond.")
	write(t, filepath.Join(root, "Caps_And.Dots", File), "---\ndescription: d\n---\nB")
	write(t, filepath.Join(root, "bad name", File), "---\nname: \"has spaces\"\ndescription: d\n---\nB")
	write(t, filepath.Join(root, "mismatch", File), "---\nname: other\ndescription: d\n---\nB")
	write(t, filepath.Join(root, "huge", File), "---\nname: huge\ndescription: d\n---\n"+strings.Repeat("x", maxFileBytes))
	write(t, filepath.Join(root, "binary", File), "---\nname: binary\n---\n\xff\xfe")
	write(t, filepath.Join(root, "long-desc", File), "---\nname: long-desc\ndescription: "+strings.Repeat("y", 2000)+"\n---\nB")
	sk, problems := Scan([]Root{{Path: root, Source: "agents", Scope: ScopeUser}}, nil)
	nd := find(sk, "no-desc")
	if nd == nil || nd.Description != "The first paragraph explains it." || len(nd.Warnings) == 0 {
		t.Fatalf("no-desc = %+v", nd)
	}
	if c := find(sk, "Caps_And.Dots"); c == nil || len(c.Warnings) < 2 {
		t.Fatalf("a name the standard wouldn't allow is kept with warnings: %+v", c)
	}
	if find(sk, "has spaces") != nil {
		t.Fatal("an unusable name must not become a skill")
	}
	if m := find(sk, "other"); m == nil || !strings.Contains(strings.Join(m.Warnings, ";"), "mismatch") {
		t.Fatalf("mismatch = %+v", m)
	}
	if find(sk, "huge") != nil || find(sk, "binary") != nil {
		t.Fatal("an oversize or non-UTF-8 SKILL.md must be refused")
	}
	if ld := find(sk, "long-desc"); ld == nil || len(ld.Description) > maxDescription+4 {
		t.Fatalf("long description not cut: %+v", ld)
	}
	if len(problems) < 3 {
		t.Fatalf("problems = %v", problems)
	}
}

func TestSkillFileCannotBeASymlinkOutOfItsFolder(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	write(t, secret, "---\nname: leak\ndescription: from outside\n---\nSECRET")
	if err := os.MkdirAll(filepath.Join(root, "leak"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak", File)); err != nil {
		t.Skip("no symlinks here")
	}
	sk, problems := Scan([]Root{{Path: root, Source: "agents", Scope: ScopeUser}}, nil)
	if len(sk) != 0 || len(problems) == 0 {
		t.Fatalf("followed a SKILL.md symlink out of its folder: %+v %v", sk, problems)
	}
}

func TestReadStripsFrontmatterAndListsFiles(t *testing.T) {
	root := t.TempDir()
	dir := skill(t, root, "deploy", "deploy", "Ship it")
	write(t, filepath.Join(dir, "scripts", "run.sh"), "#!/bin/sh")
	write(t, filepath.Join(dir, "references", "a", "b.md"), "x")
	write(t, filepath.Join(dir, ".git", "config"), "x")
	sk, _ := Scan([]Root{{Path: root, Source: "agents", Scope: ScopeUser}}, nil)
	d, err := Read(sk[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Body, "description:") || !strings.Contains(d.Body, "Do the thing.") {
		t.Fatalf("body = %q", d.Body)
	}
	if strings.Join(d.Files, ",") != "references/a/b.md,scripts/run.sh" {
		t.Fatalf("files = %v", d.Files)
	}
	if sk[0].Files != 2 {
		t.Fatalf("file count = %d", sk[0].Files)
	}
}

func TestCatalogRemembersWhatTheUserSwitchedOff(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	dir := skill(t, filepath.Join(home, ".agents", "skills"), "a", "a", "A")
	real, _ := filepath.EvalSymlinks(dir)
	c, err := NewCatalog(state, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnabled("", real, false); err != nil {
		t.Fatal(err)
	}
	if got := c.ForAgent("", Native); len(got) != 0 {
		t.Fatalf("a disabled skill is still offered: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, File)); err != nil {
		t.Fatal("switching a skill off must not touch its files")
	}
	c2, _ := NewCatalog(state, home)
	if sk := c2.Scan("").Skills; len(sk) != 1 || sk[0].Enabled {
		t.Fatalf("the switch didn't persist: %+v", sk)
	}
	if err := c2.SetEnabled("", real, true); err != nil || len(c2.ForAgent("", Native)) != 1 {
		t.Fatal("re-enabling failed")
	}
	if err := c.SetEnabled("", "/etc", false); err == nil {
		t.Fatal("only a folder a scan finds may be switched")
	}
	if err := c.SetEnabled("", filepath.Join(home, ".agents", "skills", "a", "..", ".."), false); err == nil {
		t.Fatal("a path that isn't a discovered skill's folder must be refused")
	}
}

func TestListing(t *testing.T) {
	sk := []Skill{
		{Name: "a", Description: `Does <things> & "stuff"`, File: "/s/a/SKILL.md"},
		{Name: "hidden", Description: "user only", File: "/s/h/SKILL.md", DisableModelInvocation: true},
	}
	got := Listing(sk)
	if !strings.Contains(got, "<name>a</name>") || strings.Contains(got, "hidden") {
		t.Fatalf("listing = %s", got)
	}
	if strings.Contains(got, "<things>") || !strings.Contains(got, "&lt;things&gt;") {
		t.Fatalf("a description must be escaped so it can't close the list: %s", got)
	}
	if Listing(nil) != "" || Listing(sk[1:]) != "" {
		t.Fatal("nothing to list means no section")
	}
	// A big library stays inside the budget and says what it dropped.
	var many []Skill
	for i := 0; i < 400; i++ {
		many = append(many, Skill{Name: "skill-" + strings.Repeat("n", 20) + string(rune('a'+i%26)) + strings.Repeat("0", i%7), Description: strings.Repeat("d", 800), File: "/s/x/SKILL.md"})
	}
	big := Listing(many)
	if len(big) > 13<<10 || !strings.Contains(big, "more skills aren't listed") {
		t.Fatalf("big listing is %d bytes: ends %q", len(big), big[len(big)-120:])
	}
}

func TestValidName(t *testing.T) {
	for n, ok := range map[string]bool{"pdf": true, "a-b_c.d": true, "plugin:skill": true, "": false, "-a": false, "a b": false, "a$b": false, "a/b": false, strings.Repeat("a", 65): false, "../x": false} {
		if ValidName(n) != ok {
			t.Errorf("ValidName(%q) = %v", n, !ok)
		}
	}
}

func TestClaudePluginAndSyncedRoots(t *testing.T) {
	claude := t.TempDir()
	plug := filepath.Join(claude, "plugins", "cache", "p", "1")
	mk := func(dir, name string) {
		must(t, os.MkdirAll(filepath.Join(dir, name), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d\n---\nbody\n"), 0o644))
	}
	mk(filepath.Join(plug, "skills"), "from-plugin")
	mk(filepath.Join(claude, "skills", "synced", "bucket"), "from-sync")
	manifest := `{"plugins":{"p@m":[{"installPath":"` + plug + `"},{"installPath":"/etc"}]}}`
	must(t, os.WriteFile(filepath.Join(claude, "plugins", "installed_plugins.json"), []byte(manifest), 0o644))
	got := claudeExtraRoots(claude)
	if len(got) != 2 || got[0] != filepath.Join(plug, "skills") {
		t.Fatalf("roots = %v; want the plugin's skills and the synced bucket, and nothing outside the Claude dir", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
