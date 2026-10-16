package main

import "testing"

func TestClientConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := ClientConfig{
		Connect:  "tcp://example.com:7890",
		Token:    "shh",
		CA:       "/tmp/ca.pem",
		Name:     "test-device",
		Session:  "session-main",
		DeviceID: "3f2a9c",
	}
	if err := saveClientConfig(want); err != nil {
		t.Fatalf("saveClientConfig: %v", err)
	}

	got := loadClientConfig()
	if got != want {
		t.Fatalf("loadClientConfig() = %+v, want %+v", got, want)
	}
}

func TestLoadClientConfigMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got := loadClientConfig()
	if got != (ClientConfig{}) {
		t.Fatalf("loadClientConfig() with no saved file = %+v, want zero value", got)
	}
}
