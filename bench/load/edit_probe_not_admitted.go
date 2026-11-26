package main

import "strings"

// The typed answers that refuse an edit before the product admits it. The
// view_read_only texts are the checkout and byte admission refusals, which
// state that no files were changed. The view_building text is the session
// checkout gate, which refuses a mutation while the checkout's route is not
// ready, before any handler runs. Prefixes and suffixes match the product's
// messages exactly; any other wording keeps its outcome unknown.
var (
	editProbeNoMutationPrefixes = []string{
		"view_read_only: checkout mutation was not admitted; no files were changed: ",
		"view_read_only: byte mutation admission refused; no files were changed: ",
	}
	editProbeNotRoutedPrefix = `view_building: checkout "`
	editProbeNotRoutedSuffix = `" is not fully routed yet`
)

// editProbeNoMutationRefusal returns the code of a typed reply that refused
// an edit without changing any file, or "" for any other answer.
func editProbeNoMutationRefusal(attempt Attempt) string {
	message, ok := strings.CutPrefix(attempt.Error, "RPC -32000: ")
	if attempt.Outcome != "error" || attempt.Code != -32000 || !ok {
		return ""
	}
	for _, prefix := range editProbeNoMutationPrefixes {
		if strings.HasPrefix(message, prefix) {
			return "view_read_only"
		}
	}
	if strings.HasPrefix(message, editProbeNotRoutedPrefix) && strings.HasSuffix(message, editProbeNotRoutedSuffix) && !strings.Contains(message, "\n") {
		return "view_building"
	}
	return ""
}

// editProbeRefusalCode is a refusal's typed code: the wire's error_code, or
// the code that leads a plain-text tool error ("<code>: <message>").
func editProbeRefusalCode(attempt Attempt) string {
	if code, _ := attempt.ErrorData["error_code"].(string); code != "" {
		return code
	}
	message, ok := strings.CutPrefix(attempt.Error, "RPC -32000: ")
	code, _, found := strings.Cut(message, ": ")
	if !ok || !found || code == "" || strings.ContainsAny(code, " \n") {
		return ""
	}
	return code
}

// editProbeNotAdmitted names the refusal that ended an edit's logical call
// when the product admitted none of its attempts, or returns "".
//
// LOAD re-sends an edit only after a busy or retryable refusal, which it
// already treats as not admitted. So the outcome is known when every earlier
// attempt was one of those and the call ended in a typed no-mutation reply,
// or in the backoff after such a refusal, when no further request was sent.
// A reply that never arrived, a tool deadline that abandoned running work, a
// busy reply whose delivery is unknown, and every other error stay unknown.
func editProbeNotAdmitted(attempts []Attempt) string {
	refusal := ""
	for i, attempt := range attempts {
		last := i == len(attempts)-1
		switch {
		case attempt.Outcome == "busy" && attempt.Code == -32002:
			refusal = "server_busy"
		case attempt.Outcome == "retryable":
			if refusal = editProbeRefusalCode(attempt); refusal == "" {
				return ""
			}
		case last && i > 0 && attempt.Code == 0 && attempt.DurationMS == 0 && strings.HasPrefix(attempt.Error, busyBackoffEnded):
			// The logical deadline ended while waiting to re-send.
		case last:
			refusal = editProbeNoMutationRefusal(attempt)
		default:
			return ""
		}
	}
	return refusal
}
