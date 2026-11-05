package main

import (
	"testing"

	"yoakeland/internal/protocol"
)

// set_web_tools_enabled saves a setting for the whole project, so it needs Trusted.
func TestWebToolsSettingNeedsTrusted(t *testing.T) {
	if tierMay(TierLimited, protocol.EventSetWebToolsEnabled) || !tierMay(TierTrusted, protocol.EventSetWebToolsEnabled) {
		t.Error("set_web_tools_enabled should be Trusted and up")
	}
}
