package indexer

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"weak"
)

// Passive constructor naming only; never a registry or publication authority.
// io.ReadFull is deliberately fallible for caller-supplied reader controls.
// The standard OS crypto reader's catastrophic failure remains runtime-fatal.
func sharedConstructorName(reader io.Reader) string {
	var bytes [32]byte
	if _, err := io.ReadFull(reader, bytes[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(bytes[:])
}

func (r *PublicationPhaseRecorder) initializeSharedIdentity() {
	r.sharedInstance = sharedConstructorName(rand.Reader)
	r.sharedSelf = weak.Make(r)
}
