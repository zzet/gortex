package mcp

import "testing"

func TestBootstrapTrackPathUsesFacadeLowering(t *testing.T) {
	for _, tc := range []struct{ frame, want string }{
		{`{"method":"tools/call","params":{"name":"track_repository","arguments":{"path":"/legacy"}}}`, "/legacy"},
		{`{"method":"tools/call","params":{"name":"index_repository","arguments":{"path":"/index"}}}`, "/index"},
		{`{"method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"track","arguments":{"path":"/nested"}}}}`, "/nested"},
		{`{"method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"track","arguments":{"path":"/nested"},"path":"/top"}}}`, "/top"},
		{`{"method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"track","arguments":{"path":"/nested"},"target":{"file":"/selected"}}}}`, "/selected"},
	} {
		if got, ok := BootstrapTrackPath([]byte(tc.frame)); !ok || got != tc.want {
			t.Fatalf("bootstrap path differs from tool lowering: %q %t, want %q", got, ok, tc.want)
		}
	}
	for _, frame := range []string{
		`not json`, `{"method":"tools/list"}`, `{"method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"untrack","path":"/other"}}}`,
		`{"method":"tools/call","params":{"name":"read","arguments":{"path":"/file"}}}`,
		`{"method":"tools/call","params":{"name":"track_repository","arguments":{"path":7}}}`,
	} {
		if _, ok := BootstrapTrackPath([]byte(frame)); ok {
			t.Fatalf("unrelated or malformed call treated as bootstrap tracking: %s", frame)
		}
	}
}
