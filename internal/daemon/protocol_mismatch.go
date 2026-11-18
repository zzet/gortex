package daemon

import (
	"fmt"
	"strconv"
	"strings"
)

// ProtocolMismatchError preserves the peer's identity when it rejects a
// handshake. Protocol zero is valid; DaemonProtocolKnown distinguishes it from
// a rejection too old or malformed to identify the daemon's protocol.
type ProtocolMismatchError struct {
	PeerPID             int
	DaemonInstance      string
	DaemonVersion       string
	DaemonProtocol      int
	DaemonProtocolKnown bool
	ClientProtocol      int
	Message             string
}

func (e *ProtocolMismatchError) Error() string {
	return fmt.Sprintf("%s: %s", ErrProtocolVersionMismatch, e.Message)
}

func (e *ProtocolMismatchError) Unwrap() error { return ErrProtocolVersionMismatch }

func protocolMismatch(ack HandshakeAck, clientProtocol int) *ProtocolMismatchError {
	e := &ProtocolMismatchError{
		DaemonVersion:  ack.DaemonVersion,
		DaemonInstance: ack.DaemonInstance,
		ClientProtocol: clientProtocol,
		Message:        ack.ErrorMsg,
	}
	if ack.DaemonProtocol != nil && *ack.DaemonProtocol >= 0 {
		e.DaemonProtocol, e.DaemonProtocolKnown = *ack.DaemonProtocol, true
	} else {
		e.DaemonProtocol, e.DaemonProtocolKnown = legacyDaemonProtocol(ack.ErrorMsg)
	}
	return e
}

// Deployed daemons only supplied this fixed message. Do not guess direction
// from arbitrary numbers or a malformed rejection.
func legacyDaemonProtocol(message string) (int, bool) {
	rest, ok := strings.CutPrefix(message, "daemon expects protocol ")
	if !ok {
		return 0, false
	}
	d, c, ok := strings.Cut(rest, ", client sent ")
	if !ok {
		return 0, false
	}
	daemonProtocol, dErr := strconv.Atoi(d)
	clientProtocol, cErr := strconv.Atoi(c)
	return daemonProtocol, dErr == nil && cErr == nil && daemonProtocol >= 0 && clientProtocol >= 0
}
