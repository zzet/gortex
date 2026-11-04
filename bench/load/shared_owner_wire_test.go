package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// The private retained wire contains real process and checkout identities and
// stays outside the repository. This opt-in test checks its numerical ABI;
// retained bytes cannot establish current process/member liveness or ACK work.
func TestSharedOwnerRetainedWireNumericalABI(t *testing.T) {
	path := os.Getenv("GORTEX_SHARED_OWNER_WIRE_FIXTURE")
	if path == "" {
		t.Skip("requires an explicitly supplied retained private producer wire")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var status map[string]any
	if err := decoder.Decode(&status); err != nil {
		t.Fatal(err)
	}
	proof, _ := status["shared_owner_accounting_v1"].(map[string]any)
	if !editProbeExactKeys(proof, "reference", "definition", "membership") {
		t.Fatal("retained wire does not contain the exact shared-owner envelope")
	}
	definition, _ := proof["definition"].(map[string]any)
	count, err := strconvEditProbeUint(definition["frontier_count"])
	if err != nil || count == 0 || count > uint64(len(raw)) {
		t.Fatal("invalid retained frontier count")
	}
	execution, _ := definition["execution"].(map[string]any)
	e, err := checkEditProbeSharedExecution(execution, int(count))
	if err != nil {
		t.Fatalf("actual producer E numerical ABI: %v", err)
	}
	background, _ := definition["background"].(map[string]any)
	owner, _ := definition["owner"].(map[string]any)
	b, err := checkEditProbeSharedBackground(background, owner, e)
	if err != nil {
		t.Fatalf("actual producer B numerical ABI: %v", err)
	}
	receipt, _ := status["receipt"].(string)
	unbound := checkEditProbeAccounting(status, receipt)
	if unbound.State != "unavailable" || unbound.P != nil || unbound.B != nil || len(unbound.Reasons) == 0 {
		t.Fatalf("retained numerical wire borrowed live owner/point credit: %+v", unbound)
	}
	t.Logf("numerical ABI only: E=%dns B=%dns; owner/domain/current membership and A acceptance remain separate", e.WallNS, b.WallNS)
}
