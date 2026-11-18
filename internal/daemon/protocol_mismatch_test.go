package daemon

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/testenv"
)

func TestProtocolMismatchIdentity(t *testing.T) {
	zero, two := 0, 2
	for _, tc := range []struct {
		name  string
		ack   HandshakeAck
		want  int
		known bool
	}{
		{"structured zero", HandshakeAck{DaemonVersion: "v0.64.10", DaemonProtocol: &zero}, 0, true},
		{"structured wins", HandshakeAck{DaemonProtocol: &two, ErrorMsg: "daemon expects protocol 0, client sent 1"}, 2, true},
		{"legacy zero", HandshakeAck{ErrorMsg: "daemon expects protocol 0, client sent 1"}, 0, true},
		{"legacy newer", HandshakeAck{ErrorMsg: "daemon expects protocol 2, client sent 1"}, 2, true},
		{"unknown", HandshakeAck{ErrorMsg: "protocol mismatch"}, 0, false},
		{"malformed", HandshakeAck{ErrorMsg: "daemon expects protocol -1, client sent 1"}, -1, false},
		{"trailing text", HandshakeAck{ErrorMsg: "daemon expects protocol 0, client sent 1 garbage"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := protocolMismatch(tc.ack, ProtocolVersion)
			if !errors.Is(e, ErrProtocolVersionMismatch) || e.DaemonProtocol != tc.want || e.DaemonProtocolKnown != tc.known || e.DaemonVersion != tc.ack.DaemonVersion || e.ClientProtocol != ProtocolVersion {
				t.Fatalf("mismatch identity = %+v", e)
			}
			if ShouldFallBackToEmbedded(e) {
				t.Fatal("a rejected live daemon never permits embedded fallback")
			}
		})
	}
}

func TestDialLegacyProtocolMismatchCarriesKnownZero(t *testing.T) {
	socket := filepath.Join(testenv.ShortTempDir(t), "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := bufio.NewReader(c).ReadBytes('\n'); err != nil {
			done <- err
			return
		}
		done <- WriteJSONLine(c, HandshakeAck{ErrorCode: ErrProtocolMismatch, ErrorMsg: "daemon expects protocol 0, client sent 1"})
	}()
	t.Cleanup(func() { _ = l.Close() })
	c, err := DialTo(socket, Handshake{Mode: ModeMCP})
	if c != nil {
		_ = c.Close()
		t.Fatal("legacy mismatch unexpectedly established a client")
	}
	var mismatch *ProtocolMismatchError
	if !errors.As(err, &mismatch) || !errors.Is(err, ErrProtocolVersionMismatch) || mismatch.DaemonProtocol != 0 || !mismatch.DaemonProtocolKnown || mismatch.DaemonVersion != "" {
		t.Fatalf("legacy dial = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerProtocolRejectionAnnouncesIdentity(t *testing.T) {
	srv, socket := newDaemon(t, &fakeController{})
	_, err := DialTo(socket, Handshake{Mode: ModeControl, Version: ProtocolVersion + 1})
	var mismatch *ProtocolMismatchError
	if !errors.As(err, &mismatch) || mismatch.DaemonVersion != srv.Version || !mismatch.DaemonProtocolKnown || mismatch.DaemonProtocol != ProtocolVersion || mismatch.ClientProtocol != ProtocolVersion+1 {
		t.Fatalf("server rejection identity = %v", err)
	}
	if (runtime.GOOS == "darwin" || runtime.GOOS == "linux") && mismatch.PeerPID != os.Getpid() {
		t.Fatalf("socket peer PID = %d, expected test server %d", mismatch.PeerPID, os.Getpid())
	}
	if mismatch.DaemonInstance == "" {
		t.Fatal("current server omitted rejection instance identity")
	}
}
