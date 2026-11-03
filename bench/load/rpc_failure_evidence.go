package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
)

const readSourceFailureRecords = 8
const readSourceFailureBytes = 64 << 10

type readSourceFailureKey struct{}
type readSourceFailureRecorder struct {
	mu    sync.Mutex
	count int
}

func withReadSourceFailureEvidence(ctx context.Context, enabled bool) context.Context {
	if !enabled {
		return ctx
	}
	return context.WithValue(ctx, readSourceFailureKey{}, &readSourceFailureRecorder{})
}

type readSourceFailureConfig struct {
	MaxRecords int    `json:"max_records"`
	MaxBytes   int    `json:"max_combined_wire_bytes"`
	Encoding   string `json:"encoding"`
}

// Strings are explicitly base64: []byte's JSON encoding would disagree with
// the existing reflect schema's array-of-integers interpretation.
type readSourceFailureEvidence struct {
	Version                     int             `json:"schema_version"`
	Status                      string          `json:"status"`
	WireID                      int64           `json:"wire_id"`
	RequestStatus               string          `json:"request_status"`
	ResponseStatus              string          `json:"response_status"`
	RequestBytes                int             `json:"request_bytes"`
	ResponseBytes               int             `json:"response_bytes"`
	RequestBase64               string          `json:"request_wire_base64,omitempty"`
	ResponseBase64              string          `json:"response_wire_base64,omitempty"`
	WriteCompleted              bool            `json:"write_completed"`
	IsErrorPresenceKnown        bool            `json:"is_error_presence_known"`
	IsErrorPresent              bool            `json:"is_error_present"`
	IsError                     *bool           `json:"is_error"`
	ObservedFacadePresent       bool            `json:"observed_facade_present"`
	ObservedFacadePresenceKnown bool            `json:"observed_facade_presence_known"`
	ObservedFacade              json.RawMessage `json:"observed_facade,omitempty"`
	OfferedTools                string          `json:"offered_tools,omitempty"`
	OfferedToolsMode            string          `json:"offered_tools_mode,omitempty"`
	OfferedProfileKnown         bool            `json:"offered_profile_known"`
	DaemonVersion               string          `json:"daemon_version,omitempty"`
}

// Owned by one actual call, never a connection-wide last-reply cache. Raw
// bytes survive only until classification; healthy results are discarded.
type failureWireCapture struct {
	id                                       int64
	request, response                        []byte
	requestBytes, responseBytes              int
	requestStatus, responseStatus            string
	writeCompleted, unsupported              bool
	offeredTools, offeredMode, daemonVersion string
}

func readSourceFailureCapture(ctx context.Context, r request, args map[string]any) (*readSourceFailureRecorder, *failureWireCapture) {
	recorder, _ := ctx.Value(readSourceFailureKey{}).(*readSourceFailureRecorder)
	if recorder == nil || r.Tool != "read" || args["operation"] != "source" {
		return nil, nil
	}
	return recorder, &failureWireCapture{requestStatus: "absent", responseStatus: "absent"}
}

func (r *readSourceFailureRecorder) failure(c *failureWireCapture) *readSourceFailureEvidence {
	r.mu.Lock()
	full := r.count >= readSourceFailureRecords
	if !full {
		r.count++
	}
	r.mu.Unlock()
	e := &readSourceFailureEvidence{Version: 1, WireID: c.id, RequestStatus: c.requestStatus, ResponseStatus: c.responseStatus, RequestBytes: c.requestBytes, ResponseBytes: c.responseBytes, WriteCompleted: c.writeCompleted, OfferedTools: c.offeredTools, OfferedToolsMode: c.offeredMode, OfferedProfileKnown: c.offeredTools != "" && c.offeredMode != "", DaemonVersion: c.daemonVersion}
	switch {
	case full:
		e.Status = "cap_exhausted"
	case c.unsupported:
		e.Status = "unsupported_transport"
	case c.requestStatus == "oversize" || c.responseStatus == "oversize" || len(c.request)+len(c.response) > readSourceFailureBytes:
		e.Status = "oversize"
	case c.requestStatus != "complete":
		e.Status = "request_" + c.requestStatus
	case c.responseStatus != "complete":
		e.Status = "response_" + c.responseStatus
	default:
		e.Status = "exact"
	}
	if e.Status != "exact" {
		return e // Never label prefixes, missing replies or truncation as exact.
	}
	e.RequestBase64 = base64.StdEncoding.EncodeToString(c.request)
	e.ResponseBase64 = base64.StdEncoding.EncodeToString(c.response)
	var response struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if json.Unmarshal(c.response, &response) == nil {
		e.IsErrorPresenceKnown = true
		if raw, present := response.Result["isError"]; present {
			e.IsErrorPresent = true
			var value *bool
			if json.Unmarshal(raw, &value) == nil {
				e.IsError = value
			}
		}
		if raw, present := response.Result["_meta"]; !present {
			e.ObservedFacadePresenceKnown = true
		} else {
			var meta map[string]json.RawMessage
			if json.Unmarshal(raw, &meta) == nil {
				e.ObservedFacadePresenceKnown = true
				e.ObservedFacade, e.ObservedFacadePresent = meta["gortex_facade"]
			}
		}
	}
	return e
}
