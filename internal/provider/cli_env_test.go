package provider

import (
	"slices"
	"testing"
)

func TestCLIEnv_DropsDaemonSecrets(t *testing.T) {
	t.Setenv("YOAKELAND_CLI_ENV", "GITHUB_TOKEN")
	in := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "XDG_CONFIG_HOME=/c", "CLAUDE_CONFIG_DIR=/x",
		"ANTHROPIC_API_KEY=k", "OPENAI_API_KEY=k", "OPENAI_BASE_URL=u", "YOAKELAND_MASTER_KEY=m", "AWS_SECRET_ACCESS_KEY=s", "GITHUB_TOKEN=g", "OTHER=1"}
	want := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "XDG_CONFIG_HOME=/c", "CLAUDE_CONFIG_DIR=/x", "GITHUB_TOKEN=g"}
	if got := cliEnv(in); !slices.Equal(got, want) {
		t.Fatalf("cliEnv = %v, want %v", got, want)
	}
}
