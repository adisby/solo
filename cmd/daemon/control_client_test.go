package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/solo-ai/solo/pkg/agent"
	"github.com/solo-ai/solo/pkg/config"
)

func TestReplacedControlConnectionDoesNotReconnect(t *testing.T) {
	if shouldReconnectControl(errControlConnectionReplaced) {
		t.Fatal("replaced control connection must stop reconnecting")
	}
	if shouldReconnectControl(&websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "connection replaced"}) {
		t.Fatal("replacement close frame must stop reconnecting")
	}
	if shouldReconnectControl(errControlConnectionRejected) {
		t.Fatal("permanent Server rejection must stop reconnecting")
	}
	if !shouldReconnectControl(errors.New("network unavailable")) {
		t.Fatal("ordinary network failure must reconnect")
	}
}

func TestDaemonCredentialIsScopedToServerOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	t.Setenv("SOLO_DAEMON_CREDENTIAL_FILE", path)
	if err := saveDaemonCredential(daemonCredential{
		ServerURL: "https://solo.example.com", ComputerID: "computer-1", Secret: "secret",
	}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	// Windows has no mode bits: the platform check reads the ACL and requires
	// that the inherited grants every local account could use are gone.
	if !agent.HasOwnerOnlyAccess(path) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat credential: %v", err)
		}
		t.Fatalf("credential is readable beyond its owner: mode=%v", info.Mode().Perm())
	}

	t.Setenv("DAEMON_SERVER_URL", "http://127.0.0.1:8080")
	if _, err := newDaemonControlClient(&config.Config{ServerURL: "http://127.0.0.1:8080"}, nil, nil); err == nil {
		t.Fatal("credential from another Server origin was reused")
	}

	t.Setenv("DAEMON_SERVER_URL", "https://solo.example.com")
	client, err := newDaemonControlClient(&config.Config{ServerURL: "https://solo.example.com"}, nil, nil)
	if err != nil {
		t.Fatalf("matching Server credential rejected: %v", err)
	}
	if client.computer.ComputerID != "computer-1" || client.computer.Secret != "secret" {
		t.Fatalf("matching credential not loaded: %#v", client.computer)
	}
}

func TestDaemonCredentialAllowsCustomFileInSharedParent(t *testing.T) {
	file, err := os.CreateTemp("", "solo-daemon-credential-*.json")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	t.Setenv("SOLO_DAEMON_CREDENTIAL_FILE", path)

	if err := saveDaemonCredential(daemonCredential{ServerURL: "https://solo.example.com", ComputerID: "computer-1", Secret: "secret"}); err != nil {
		t.Fatalf("save credential under shared parent: %v", err)
	}
	if !agent.HasOwnerOnlyAccess(path) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("credential mode = %v, want owner-only", info.Mode().Perm())
	}
}
