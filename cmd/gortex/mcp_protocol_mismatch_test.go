package main

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/testenv"
)

// This is a real rejected handshake, including availability probes that close
// before sending a frame. It never starts or touches a user daemon.
func serveMismatchAck(t *testing.T, ack daemon.HandshakeAck) string {
	t.Helper()
	socket := filepath.Join(testenv.ShortTempDir(t), "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active := make(map[net.Conn]struct{})
	var conns sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			active[c] = struct{}{}
			mu.Unlock()
			conns.Add(1)
			go func() {
				defer conns.Done()
				defer func() { _ = c.Close(); mu.Lock(); delete(active, c); mu.Unlock() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := bufio.NewReader(c).ReadBytes('\n'); err == nil {
					_ = daemon.WriteJSONLine(c, ack)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-stopped
		mu.Lock()
		for c := range active {
			_ = c.Close()
		}
		mu.Unlock()
		conns.Wait()
	})
	return socket
}

func TestRunMCPMismatchNeverCreatesEmbeddedStore(t *testing.T) {
	zero, two := 0, daemon.ProtocolVersion+1
	for _, tc := range []struct {
		name             string
		protocol         *int
		version, message string
	}{
		{"older", &zero, "v0.64.10", "daemon expects protocol 0, client sent 1"},
		{"legacy", nil, "", "daemon expects protocol 0, client sent 1"},
		{"newer protocol", &two, "v0.64.10", "daemon expects protocol 2, client sent 1"},
		{"newer release", &zero, "v0.64.13", "daemon expects protocol 0, client sent 1"},
	} {
		for _, allow := range []bool{false, true} {
			name := tc.name + "/deny"
			if allow {
				name = tc.name + "/allow"
			}
			t.Run(name, func(t *testing.T) {
				testenv.Sandbox(t)
				tmp := redirectTempRoot(t)
				repo := filepath.Join(tmp, "repo")
				if err := os.Mkdir(repo, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, "example.go"), []byte("package example\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Chdir(repo)
				stubUnavailableMCPDaemon(t)
				t.Cleanup(restoreSeams)
				if strings.HasPrefix(tc.name, "newer") {
					t.Setenv("GORTEX_AUTOSTART", "1")
				}
				stops, spawns := 0, 0
				stopStaleDaemon = func(daemonIdentity) error {
					stops++
					return errors.New("unexpected replacement stop")
				}
				spawnDaemon = func() error {
					spawns++
					return errors.New("unexpected replacement spawn")
				}
				binaryVersion = func() string { return "v0.64.12" }
				isDaemonRunning = daemon.IsRunning
				runningDaemonInfo = dialRunningDaemonIdentity
				probeDaemonAvailability = daemon.ProbeAvailability
				socket := serveMismatchAck(t, daemon.HandshakeAck{ErrorCode: daemon.ErrProtocolMismatch, ErrorMsg: tc.message, DaemonVersion: tc.version, DaemonProtocol: tc.protocol})
				t.Setenv("GORTEX_DAEMON_SOCKET", socket)
				path := config.DefaultGlobalConfigPath()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				value := "false"
				if allow {
					value = "true"
				}
				if err := os.WriteFile(path, []byte("mcp:\n  allow_embedded: "+value+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := &cobra.Command{}
				cmd.SetContext(t.Context())
				err := runMCP(cmd, nil)
				if !errors.Is(err, daemon.ErrProtocolVersionMismatch) {
					t.Fatalf("runMCP = %v", err)
				}
				if stops != 0 || spawns != 0 || !daemon.IsRunningAt(socket) {
					t.Fatalf("refusal disturbed running daemon: stops=%d spawns=%d", stops, spawns)
				}
				for _, want := range []string{"protocol", "CLI v0.64.12", "daemon "} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("diagnostic %q missing %q", err, want)
					}
				}
				matches, globErr := filepath.Glob(filepath.Join(tmp, embeddedStoreDirPattern))
				if globErr != nil || len(matches) != 0 {
					t.Fatalf("mismatch allocated embedded store: %v (%v)", matches, globErr)
				}
				if _, err := os.Stat(filepath.Join(repo, ".gortex")); !os.IsNotExist(err) {
					t.Fatalf("mismatch created repository state: %v", err)
				}
			})
		}
	}
}

func TestRunMCPObservedMismatchSurvivesReadyThenUnavailableProxy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "deny"
		if allow {
			name = "allow"
		}
		t.Run(name, func(t *testing.T) {
			testenv.Sandbox(t)
			tmp := redirectTempRoot(t)
			t.Chdir(tmp)
			stubUnavailableMCPDaemon(t)
			t.Cleanup(restoreSeams)
			zero := 0
			socket := serveMismatchAck(t, daemon.HandshakeAck{
				ErrorCode: daemon.ErrProtocolMismatch, ErrorMsg: "daemon expects protocol 0, client sent 1",
				DaemonVersion: "v0.64.10", DaemonProtocol: &zero,
			})
			_, observed := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeControl})
			if !errors.Is(observed, daemon.ErrProtocolVersionMismatch) {
				t.Fatalf("initial wire refusal = %v", observed)
			}
			probeDaemonAvailability = func() error { return observed }
			isDaemonRunning = func() bool { return true }
			binaryVersion = func() string { return "v0.64.12" }
			runningDaemonInfo = func() daemonIdentity {
				return daemonIdentity{version: "v0.64.12", protocol: daemon.ProtocolVersion, protocolKnown: true}
			}
			withFastDialRetry(t, 5*time.Millisecond)
			proxyCalls := 0
			dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
				proxyCalls++
				return nil, daemon.ErrDaemonUnavailable
			}
			path := config.DefaultGlobalConfigPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			value := "false"
			if allow {
				value = "true"
			}
			if err := os.WriteFile(path, []byte("mcp:\n  allow_embedded: "+value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			err := runMCP(cmd, nil)
			if !errors.Is(err, daemon.ErrProtocolVersionMismatch) || proxyCalls == 0 {
				t.Fatalf("ready then unavailable proxy = %v (dials %d)", err, proxyCalls)
			}
			matches, globErr := filepath.Glob(filepath.Join(tmp, embeddedStoreDirPattern))
			if globErr != nil || len(matches) != 0 {
				t.Fatalf("observed mismatch allocated embedded store: %v (%v)", matches, globErr)
			}
		})
	}
}

func TestRunMCPHandoffMismatchSurvivesUnavailableProxy(t *testing.T) {
	for _, initiallyCompatible := range []bool{false, true} {
		phase := "mismatch-first"
		if initiallyCompatible {
			phase = "compatible-then-mismatch-under-lock"
		}
		t.Run(phase, func(t *testing.T) {
			for _, allow := range []bool{false, true} {
				name := "deny"
				if allow {
					name = "allow"
				}
				t.Run(name, func(t *testing.T) {
					testenv.Sandbox(t)
					tmp := redirectTempRoot(t)
					t.Chdir(tmp)
					stubUnavailableMCPDaemon(t)
					t.Setenv("GORTEX_AUTOSTART", "1")
					t.Cleanup(restoreSeams)
					zero := 0
					socket := serveMismatchAck(t, daemon.HandshakeAck{ErrorCode: daemon.ErrProtocolMismatch, ErrorMsg: "daemon expects protocol 0, client sent 1", DaemonVersion: "v0.64.10", DaemonProtocol: &zero})
					_, wireErr := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeControl})
					old := daemonIdentityFromMismatch(wireErr)
					if !old.mismatch() {
						t.Fatalf("initial wire refusal = %v", wireErr)
					}
					probeDaemonAvailability = func() error { return nil } // actual availability is socket-only
					binaryVersion = func() string { return "v0.64.12" }
					alive, replaced := true, false
					infoCalls := 0
					stoppedMismatch := false
					isDaemonRunning = func() bool { return alive }
					runningDaemonInfo = func() daemonIdentity {
						infoCalls++
						if initiallyCompatible && infoCalls == 1 {
							return daemonIdentity{version: "v0.64.10", protocol: daemon.ProtocolVersion, protocolKnown: true}
						}
						if !alive {
							return daemonIdentity{err: daemon.ErrDaemonUnavailable}
						}
						if replaced {
							return daemonIdentity{version: "v0.64.12", protocol: daemon.ProtocolVersion, protocolKnown: true}
						}
						return old
					}
					serviceActive = func() bool { return false }
					stopStaleDaemon = func(identity daemonIdentity) error {
						stoppedMismatch = identity.mismatch()
						alive = false
						return nil
					}
					spawnDaemon = func() error { alive, replaced = true, true; return nil }
					withFastDialRetry(t, 5*time.Millisecond)
					proxyCalls := 0
					dialDaemon = func(daemon.Handshake) (*daemon.Client, error) { proxyCalls++; return nil, daemon.ErrDaemonUnavailable }
					path := config.DefaultGlobalConfigPath()
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					value := "false"
					if allow {
						value = "true"
					}
					if err := os.WriteFile(path, []byte("mcp:\n  allow_embedded: "+value+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					cmd := &cobra.Command{}
					cmd.SetContext(t.Context())
					err := runMCP(cmd, nil)
					if !errors.Is(err, daemon.ErrProtocolVersionMismatch) || !replaced || !stoppedMismatch || proxyCalls == 0 {
						t.Fatalf("post-handoff proxy disappearance = %v (replaced=%v, dials=%d)", err, replaced, proxyCalls)
					}
					if !strings.Contains(err.Error(), "v0.64.10 (protocol 0)") {
						t.Fatalf("lost actual replacement mismatch identity: %v", err)
					}
					matches, globErr := filepath.Glob(filepath.Join(tmp, embeddedStoreDirPattern))
					if globErr != nil || len(matches) != 0 {
						t.Fatalf("handoff mismatch allocated embedded store: %v (%v)", matches, globErr)
					}
				})
			}
		})
	}
}
