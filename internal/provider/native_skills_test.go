package provider

import "testing"

func TestSkillPromptPerAgent(t *testing.T) {
	cases := []struct {
		kind   CLIKind
		prompt string
		names  []string
		want   string
	}{
		{CLIKindClaude, "$polish the README", []string{"polish"}, "/polish the README"},
		{CLIKindClaude, "please $polish the README, it costs $5", []string{"polish"}, "/polish please the README, it costs $5"},
		{CLIKindClaude, "$polish", []string{"polish"}, "/polish"},
		{CLIKindClaude, "first $a then $b now", []string{"a", "b"}, "/b first /a then now"},
		{CLIKindCodex, "$polish the README", []string{"polish"}, "$polish the README"},
		{CLIKindPi, "do $review on this", []string{"review"}, "/skill:review do on this"},
		{CLIKindOhMyPi, "$review", []string{"review"}, "/skill:review"},
		{CLIKindOpenCode, "run $deploy now", []string{"deploy"}, "Use the deploy skill for this.\n\nrun deploy now"},
		{CLIKindAmp, "$a and $b", []string{"a", "b"}, "Use the a, b skills for this.\n\na and b"},
	}
	for _, c := range cases {
		p := &CLISubprocessProvider{Kind: c.kind}
		if got := p.SkillPrompt(c.prompt, c.names); got != c.want {
			t.Errorf("%s %q = %q, want %q", c.kind, c.prompt, got, c.want)
		}
	}
	if (&CLISubprocessProvider{Kind: CLIKindOhMyPi}).SkillKind() != "ohmypi" || (&CLISubprocessProvider{Kind: CLIKindClaude}).SkillKind() != "claude" {
		t.Error("SkillKind must match the names the skill folders use")
	}
}
