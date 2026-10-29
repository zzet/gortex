package platform

import "testing"

func TestProcessBootScopeRejectsMissingOrMalformedLinuxIdentity(t *testing.T) {
	const valid = "12345678-ABCD-1234-5678-123456789ABC"
	got, err := normalizeProcessBootScope([]byte(valid + "\n"))
	if err != nil || got != "12345678-abcd-1234-5678-123456789abc" {
		t.Fatalf("valid boot identity: %q (%v)", got, err)
	}
	for _, value := range []string{"", "unknown", "12345678_1234-1234-1234-123456789abc", "12345678-zzzz-1234-1234-123456789abc", valid + valid} {
		if _, err := normalizeProcessBootScope([]byte(value)); err == nil {
			t.Fatalf("accepted unknown Linux boot scope %q", value)
		}
	}
}
