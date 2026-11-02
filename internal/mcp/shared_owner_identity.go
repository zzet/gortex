package mcp

import (
 "crypto/rand"
 "encoding/hex"
 "io"
 "os"
 "weak"

 "github.com/zzet/gortex/internal/platform"
)

type sharedServerIdentity struct {
 name string
 self weak.Pointer[Server]
 pid int
 birth string
}

func sharedServerConstructorName(reader io.Reader) string {
 var bytes [32]byte
 if _, err := io.ReadFull(reader, bytes[:]); err != nil { return "" }
 return hex.EncodeToString(bytes[:])
}

func (s *Server) initializeSharedIdentity() {
 s.sharedIdentity = sharedServerIdentity{name: sharedServerConstructorName(rand.Reader), self: weak.Make(s), pid: os.Getpid()}
 native, err := platform.CaptureProcessIdentity(s.sharedIdentity.pid)
 if err != nil { return }
 s.sharedIdentity.birth, _ = native.Token()
}

func (s *Server) validSharedIdentity() bool {
 if s.sharedIdentity.name == "" || s.sharedIdentity.self.Value() != s || s.sharedIdentity.pid != os.Getpid() || s.sharedIdentity.birth == "" { return false }
 native, err := platform.CaptureProcessIdentity(s.sharedIdentity.pid)
 if err != nil { return false }
 token, err := native.Token()
 if err != nil || token != s.sharedIdentity.birth { return false }
 running, err := native.Running()
 return err == nil && running
}
