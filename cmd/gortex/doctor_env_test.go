package main

import (
	"bytes"
	"testing"
)

// TestPrintDoctorEnvironment_VersionSkew — the daemon handshake row
// carries ✓ when CLI and daemon agree and ! plus the skew remedy line
// when they don't, reusing daemonSkewWarning so doctor, `daemon
// status`, and the MCP proxy render one verdict.
func TestPrintDoctorEnvironment_VersionSkew(t *testing.T) {
	t.Run("matching versions keep the check row", func(t *testing.T) {
		env := DoctorEnvironment{
			DaemonRunning: true,
			DaemonVersion: "v0.63.4+5f5fce2",
			CLIVersion:    "v0.63.4+5f5fce2",
			DaemonSocket:  "/tmp/gortex.sock",
		}
		if warn := daemonSkewWarning(env.DaemonVersion, env.CLIVersion); warn != "" {
			t.Fatalf("precondition: expected no skew warning for matching versions, got %q", warn)
		}
		var buf bytes.Buffer
		printDoctorEnvironment(&buf, env)
		out := buf.String()
		if !bytes.Contains([]byte(out), []byte("✓ daemon handshake: v0.63.4+5f5fce2")) {
			t.Fatalf("expected check-marked handshake row, got:\n%s", out)
		}
		if bytes.Contains([]byte(out), []byte("warning:")) {
			t.Fatalf("unexpected skew warning in matching-version render:\n%s", out)
		}
	})

	t.Run("skewed versions warn with the remedy", func(t *testing.T) {
		env := DoctorEnvironment{
			DaemonRunning: true,
			DaemonVersion: "v0.63.4+5f9ce2a",
			CLIVersion:    "v0.63.5+abcdef1",
			DaemonSocket:  "/tmp/gortex.sock",
		}
		env.VersionSkewWarning = daemonSkewWarning(env.DaemonVersion, env.CLIVersion)
		if env.VersionSkewWarning == "" {
			t.Fatal("precondition: daemonSkewWarning must fire for skewed versions")
		}
		var buf bytes.Buffer
		printDoctorEnvironment(&buf, env)
		out := buf.String()
		if !bytes.Contains([]byte(out), []byte("! daemon handshake: v0.63.4+5f9ce2a")) {
			t.Fatalf("expected warn-marked handshake row, got:\n%s", out)
		}
		if !bytes.Contains([]byte(out), []byte("run 'gortex daemon restart' to upgrade the daemon")) {
			t.Fatalf("expected the skew remedy line, got:\n%s", out)
		}
	})
}
