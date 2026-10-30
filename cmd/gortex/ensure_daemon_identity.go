package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	semver "github.com/zzet/gortex/internal/version"
)

type daemonIdentity struct {
	version       string
	protocol      int
	protocolKnown bool
	instance      string
	peerPID       int
	err           error
}

func dialRunningDaemonIdentity() daemonIdentity {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return daemonIdentityFromMismatch(err)
	}
	defer c.Close()
	// A successful handshake proves protocol equality even when an older
	// daemon omitted the new identity field.
	return daemonIdentity{version: c.Ack.DaemonVersion, protocol: daemon.ProtocolVersion, protocolKnown: true, instance: c.Ack.DaemonInstance, peerPID: c.PeerPID}
}

// Newer in either known dimension vetoes replacement, including contradictory
// release/protocol directions. Unknown release versions cannot establish a
// release direction, but a known older protocol remains upgrade evidence.
func (d daemonIdentity) olderThan(local string) bool {
	if d.protocolKnown && d.protocol > daemon.ProtocolVersion {
		return false
	}
	olderRelease := false
	if local != "" && local != "v0.0.0-dev" && d.version != "" && d.version != "v0.0.0-dev" {
		dv, dErr := semver.Parse(d.version)
		lv, lErr := semver.Parse(local)
		if dErr == nil && lErr == nil {
			cmp := semver.Compare(dv, lv)
			if cmp > 0 {
				return false
			}
			olderRelease = cmp < 0
		}
	}
	return d.protocolKnown && d.protocol < daemon.ProtocolVersion || olderRelease
}

func (d daemonIdentity) mismatch() bool {
	return errors.Is(d.err, daemon.ErrProtocolVersionMismatch)
}

func (d daemonIdentity) protocolLabel() string {
	if !d.protocolKnown {
		return "unknown"
	}
	return fmt.Sprint(d.protocol)
}

func daemonVersionLabel(version string) string {
	if version == "" {
		return "unknown"
	}
	// Peer-supplied identity must not turn one diagnostic into multiple lines.
	version = strings.Join(strings.Fields(version), " ")
	if len(version) > 128 {
		version = version[:128]
	}
	return version
}

func daemonMismatchError(d daemonIdentity) error {
	local := binaryVersion()
	remedy := "use a binary matching the running daemon"
	if d.olderThan(local) {
		remedy = "run `gortex daemon restart`"
	} else if d.protocolKnown && d.protocol > daemon.ProtocolVersion {
		remedy = "this CLI is older than the running daemon; use the matching binary"
	} else if dv, de := semver.Parse(d.version); de == nil {
		if lv, le := semver.Parse(local); le == nil && semver.Compare(dv, lv) > 0 {
			remedy = "this CLI is older than the running daemon; use the matching binary"
		}
	}
	return fmt.Errorf("%w: daemon %s (protocol %s), CLI %s (protocol %d); %s", daemon.ErrProtocolVersionMismatch,
		daemonVersionLabel(d.version), d.protocolLabel(), daemonVersionLabel(local), daemon.ProtocolVersion, remedy)
}

func daemonHandoffError(previous daemonIdentity, stage string, err error) error {
	if previous.mismatch() {
		return fmt.Errorf("%w; replacement failed at %s: %w", daemonMismatchError(previous), stage, err)
	}
	return fmt.Errorf("replace daemon %s (protocol %s) with CLI %s (protocol %d), %s: %w; run `gortex daemon restart`", daemonVersionLabel(previous.version), previous.protocolLabel(), daemonVersionLabel(binaryVersion()), daemon.ProtocolVersion, stage, err)
}

// Keep the existing proxy startup cutoff. Hold the spawn lock while waiting
// so a supervisor's asynchronous restart cannot trigger another replacement.
func waitForReplacementDaemon() (daemonIdentity, daemonIdentity, error) {
	deadline := time.Now().Add(proxyDialRetryWindow)
	var observedMismatch daemonIdentity
	for {
		current := runningDaemonInfo()
		if current.mismatch() {
			observedMismatch = current
		}
		if current.err == nil && current.protocolKnown && current.protocol == daemon.ProtocolVersion && !current.olderThan(binaryVersion()) {
			return current, observedMismatch, nil
		}
		if current.mismatch() && !current.olderThan(binaryVersion()) {
			return current, observedMismatch, daemonMismatchError(current)
		}
		if !time.Now().Before(deadline) {
			return current, observedMismatch, context.DeadlineExceeded
		}
		time.Sleep(proxyDialRetryInterval)
	}
}

func daemonIdentityFromMismatch(err error) daemonIdentity {
	var mismatch *daemon.ProtocolMismatchError
	if errors.As(err, &mismatch) {
		return daemonIdentity{version: mismatch.DaemonVersion, protocol: mismatch.DaemonProtocol, protocolKnown: mismatch.DaemonProtocolKnown, instance: mismatch.DaemonInstance, peerPID: mismatch.PeerPID, err: err}
	}
	return daemonIdentity{err: err}
}
