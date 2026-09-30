package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagedDaemonConnectedRequiresReadyControlChannel(t *testing.T) {
	ready := false
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if ready {
			w.Write([]byte(`{"control_connected":true}`))
			return
		}
		w.Write([]byte(`{"control_connected":false}`))
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	t.Setenv("DAEMON_PORT", port)

	if managedDaemonConnected() {
		t.Fatal("Daemon reported connected before the remote control channel was ready")
	}
	ready = true
	if !managedDaemonConnected() {
		t.Fatal("Daemon did not report the ready remote control channel")
	}
}

func TestStartManagedDaemonUsesSoloStateDirectory(t *testing.T) {
	home := t.TempDir()
	callerDir := t.TempDir()
	observedCWD := filepath.Join(t.TempDir(), "cwd")
	observedCredential := filepath.Join(t.TempDir(), "credential")
	helper := filepath.Join(t.TempDir(), "solo-daemon")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\npwd > \"$OBSERVED_CWD\"\nprintf '%s\\n' \"$SOLO_DAEMON_CREDENTIAL_FILE\" > \"$OBSERVED_CREDENTIAL\"\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(callerDir, ".env"), []byte("DAEMON_SERVER_URL=http://wrong.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(callerDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	t.Setenv("HOME", home)
	t.Setenv("SOLO_DAEMON_BINARY", helper)
	t.Setenv("OBSERVED_CWD", observedCWD)
	t.Setenv("OBSERVED_CREDENTIAL", observedCredential)
	t.Setenv("SOLO_DAEMON_CREDENTIAL_FILE", "relative-credentials.json")

	if err := startManagedDaemon(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pid, running := daemonPID(); running {
			if process, findErr := os.FindProcess(pid); findErr == nil {
				_ = process.Kill()
			}
		}
		if pidPath, pathErr := daemonStatePath("daemon.pid"); pathErr == nil {
			_ = os.Remove(pidPath)
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	var raw []byte
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(observedCWD)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("read child working directory: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(home, ".solo", "daemon"))
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Clean(strings.TrimSpace(string(raw))); got != want {
		t.Fatalf("child working directory = %q, want %q", got, want)
	}
	raw, err = os.ReadFile(observedCredential)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCallerDir, err := filepath.EvalSymlinks(callerDir)
	if err != nil {
		t.Fatal(err)
	}
	wantCredential := filepath.Join(resolvedCallerDir, "relative-credentials.json")
	if got := strings.TrimSpace(string(raw)); got != wantCredential {
		t.Fatalf("child credential path = %q, want %q", got, wantCredential)
	}
}

func TestDaemonProfilesUseIndependentState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	alpha, err := daemonProfileStateDir("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := daemonProfileStateDir("beta")
	if err != nil {
		t.Fatal(err)
	}
	if alpha == beta || !strings.HasSuffix(alpha, filepath.Join("daemons", "alpha")) || !strings.HasSuffix(beta, filepath.Join("daemons", "beta")) {
		t.Fatalf("profiles share state: alpha=%q beta=%q", alpha, beta)
	}
	profile, explicit, remaining, err := parseDaemonProfile([]string{"--server", "https://solo.example", "--profile", "alpha"})
	if err != nil || profile != "alpha" || !explicit || len(remaining) != 2 {
		t.Fatalf("profile parse = %q %#v %v", profile, remaining, err)
	}
}

func TestDaemonConnectProfileUsesComputerIDUnlessExplicitOrLegacyDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	computerID := "11111111-1111-4111-8111-111111111111"
	if got := daemonConnectProfile(defaultDaemonProfile, computerID, false); got != computerID {
		t.Fatalf("new automatic profile = %q, want computer ID", got)
	}
	if got := daemonConnectProfile("work", computerID, true); got != "work" {
		t.Fatalf("explicit profile = %q, want work", got)
	}
	path, err := managedCredentialPath()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(managedDaemonCredential{ComputerID: computerID})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := daemonConnectProfile(defaultDaemonProfile, computerID, false); got != defaultDaemonProfile {
		t.Fatalf("legacy default profile = %q, want default", got)
	}
}

func TestDaemonProfilePIDRecoversFromLiveLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lockPath, err := daemonStatePath("lock.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]int{"pid": os.Getpid()})
	if err := os.WriteFile(lockPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, running := daemonPID(); !running || pid != os.Getpid() {
		t.Fatalf("daemonPID() = %d, %v; want current process", pid, running)
	}
	pidPath, err := daemonStatePath("daemon.pid")
	if err != nil {
		t.Fatal(err)
	}
	if raw, err = os.ReadFile(pidPath); err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("repaired pid record = %q, %v", raw, err)
	}
}

// setDaemonTestHome points the per-user state directory at a temp dir on both
// platforms: os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows.
func setDaemonTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("DAEMON_PORT", "")
}

func writeDaemonProfileRecord(t *testing.T, profile, name, value string) string {
	t.Helper()
	path, err := daemonProfileStatePath(profile, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// overrideDefaultDaemonPort stands the built-in default in for an ephemeral
// port, so a test never needs to bind the real 8081.
func overrideDefaultDaemonPort(t *testing.T, port int) {
	t.Helper()
	previous := defaultDaemonPort
	defaultDaemonPort = port
	t.Cleanup(func() { defaultDaemonPort = previous })
}

// startStandInDaemon serves a real Daemon health document on a loopback port.
func startStandInDaemon(t *testing.T, pid int, connected bool) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	doc := daemonHealthDoc{ControlConnected: connected, PID: pid, Port: port}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return port
}

// freeLoopbackPort returns a port nothing is listening on.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestDaemonStatusDiscoversDaemonThatOutgrewItsPortRecord(t *testing.T) {
	setDaemonTestHome(t, t.TempDir())
	const profile = "alpha"
	writeDaemonProfileRecord(t, profile, "daemon.pid", strconv.Itoa(os.Getpid())+"\n")
	stalePort := freeLoopbackPort(t)
	writeDaemonProfileRecord(t, profile, "port", strconv.Itoa(stalePort)+"\n")
	// The Daemon was started without DAEMON_PORT, so it bound the built-in
	// default while the record still named the port an earlier launch held.
	standInPort := startStandInDaemon(t, os.Getpid(), true)
	overrideDefaultDaemonPort(t, standInPort)

	endpoint, ok := managedDaemonEndpoint(profile, os.Getpid())
	if !ok || !endpoint.connected {
		t.Fatalf("managedDaemonEndpoint() = %#v, %v; want the answering Daemon", endpoint, ok)
	}
	if endpoint.port != standInPort || !endpoint.verified || endpoint.repairedFrom != stalePort {
		t.Fatalf("endpoint = %#v; want port %d verified, repaired from %d", endpoint, standInPort, stalePort)
	}
	recorded, recordedOK := recordedDaemonPort(profile)
	if !recordedOK || recorded != standInPort {
		t.Fatalf("recorded port = %d, %v; want the repaired %d", recorded, recordedOK, standInPort)
	}
	if !managedDaemonConnectedProfile(profile) {
		t.Fatal("status still reports the profile's Daemon as connecting")
	}
}

func TestDaemonStatusRejectsAnotherDaemonsAnswer(t *testing.T) {
	setDaemonTestHome(t, t.TempDir())
	const profile = "alpha"
	writeDaemonProfileRecord(t, profile, "daemon.pid", strconv.Itoa(os.Getpid())+"\n")
	foreignPort := startStandInDaemon(t, os.Getpid()+1, true)
	writeDaemonProfileRecord(t, profile, "port", strconv.Itoa(foreignPort)+"\n")
	overrideDefaultDaemonPort(t, freeLoopbackPort(t))

	if endpoint, ok := managedDaemonEndpoint(profile, os.Getpid()); ok {
		t.Fatalf("accepted a Daemon that reported another pid: %#v", endpoint)
	}
	if managedDaemonConnectedProfile(profile) {
		t.Fatal("reported another Daemon's control channel as this profile's")
	}
}

func TestDaemonStatusDoesNotAllocateAPortRecord(t *testing.T) {
	setDaemonTestHome(t, t.TempDir())
	const profile = "alpha"
	overrideDefaultDaemonPort(t, freeLoopbackPort(t))
	portPath, err := daemonProfileStatePath(profile, "port")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(portPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if managedDaemonConnectedProfile(profile) {
		t.Fatal("reported a connected Daemon with nothing listening")
	}
	if _, err := os.Stat(portPath); !os.IsNotExist(err) {
		t.Fatalf("observing a Daemon wrote a port record: %v", err)
	}
}

func TestDaemonStatusTrustsRecordedPortForOlderDaemon(t *testing.T) {
	setDaemonTestHome(t, t.TempDir())
	const profile = "alpha"
	// An older Daemon reports neither pid nor port, so only the recorded port
	// carries any evidence of which Daemon is answering.
	recordedPort := startStandInDaemon(t, 0, true)
	writeDaemonProfileRecord(t, profile, "port", strconv.Itoa(recordedPort)+"\n")
	overrideDefaultDaemonPort(t, freeLoopbackPort(t))

	endpoint, ok := managedDaemonEndpoint(profile, os.Getpid())
	if !ok || !endpoint.connected || endpoint.verified || endpoint.port != recordedPort {
		t.Fatalf("endpoint = %#v, %v; want the recorded port trusted but unverified", endpoint, ok)
	}
}

func TestDaemonStatusDoesNotTrustAGuessedPort(t *testing.T) {
	setDaemonTestHome(t, t.TempDir())
	const profile = "alpha"
	writeDaemonProfileRecord(t, profile, "port", strconv.Itoa(freeLoopbackPort(t))+"\n")
	overrideDefaultDaemonPort(t, startStandInDaemon(t, 0, true))

	if endpoint, ok := managedDaemonEndpoint(profile, os.Getpid()); ok {
		t.Fatalf("trusted an unidentified Daemon found by guessing a port: %#v", endpoint)
	}
}

func TestDaemonStatusRejectsAnswerForAPortItDoesNotHold(t *testing.T) {
	port := startStandInDaemon(t, os.Getpid(), true)
	if _, ok := probeDaemonEndpoint(port, os.Getpid(), true); !ok {
		t.Fatal("a Daemon holding the probed port was rejected")
	}
	misreported := daemonHealthDoc{ControlConnected: true, PID: os.Getpid(), Port: port + 1}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(misreported)
	}))
	defer server.Close()
	misreportedPort, err := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := probeDaemonEndpoint(misreportedPort, os.Getpid(), true); ok {
		t.Fatal("accepted a health document that does not hold the probed port")
	}
}

func TestDaemonLogNoteReportsLogFromAnEarlierRun(t *testing.T) {
	setDaemonTestHome(t, t.TempDir())
	const profile = "alpha"
	writeDaemonProfileRecord(t, profile, "daemon.pid", strconv.Itoa(os.Getpid())+"\n")
	logPath := writeDaemonProfileRecord(t, profile, "daemon.log", "{\"msg\":\"previous run\"}\n")
	writeDaemonProfileRecord(t, profile, "lock.json", `{"pid":`+strconv.Itoa(os.Getpid())+`}`)
	started, ok := daemonStartRecordTime(profile)
	if !ok {
		t.Fatal("no Daemon start record was found")
	}

	// A Daemon the CLI started writes the log after its own start records; a
	// Daemon started by the logon task has no redirection and never writes it.
	older := started.Add(-time.Hour)
	if err := os.Chtimes(logPath, older, older); err != nil {
		t.Fatal(err)
	}
	if note := staleDaemonLogNote(profile, logPath); note == "" {
		t.Fatal("a log older than the running Daemon was presented as current")
	}

	fresh := started.Add(time.Minute)
	if err := os.Chtimes(logPath, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if note := staleDaemonLogNote(profile, logPath); note != "" {
		t.Fatalf("a current log was reported as stale: %q", note)
	}
}
