//go:build !linux

package main

// parentProcessCWD has no implementation off Linux yet; the proxy then keeps
// the handshake CWD for the whole session, as it always has.
func parentProcessCWD() string { return "" }
