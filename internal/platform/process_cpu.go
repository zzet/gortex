package platform

import "errors"

// ErrProcessCPUUnavailable means this platform has no supported process CPU
// endpoint. A failed/unavailable sample must not become a measured zero.
var ErrProcessCPUUnavailable = errors.New("process CPU time unavailable on this platform")

var errInvalidProcessCPUTime = errors.New("invalid process CPU time")
