package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPDispatchBusyOccupancyFreezesExactActiveOwners(t *testing.T) {
	coordinator := mcpDispatchCoordinator{observeOccupancy: true}
	pool := coordinator.pool(newMCPDispatchSlots(4))
	acquire := func(class MCPDispatchClass, session, name, operation string) *mcpScheduledGrant {
		t.Helper()
		args := map[string]any{"operation": operation, "target": "private-target"}
		ctx := withMCPDispatchRequest(t.Context(), mcpDispatchRequestInfo{
			class: class, session: session, occupant: mcpDispatchOccupantFor(name, args),
		})
		grant, err := pool.acquire(ctx, class, session)
		if err != nil {
			t.Fatalf("holder %s failed to enter: %v", name, err)
		}
		t.Cleanup(grant.complete)
		return grant
	}
	acquire(MCPDispatchFast, "private-read-session", "read", "summary")
	receipt := acquire(MCPDispatchFast, "private-status-session", "change", "receipt")
	acquire(MCPDispatchMutating, "private-write-session", "edit", "file")
	protocol, err := pool.acquireProtocol(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(protocol.complete)

	refuse := func() error {
		t.Helper()
		info := mcpDispatchRequestInfo{
			class: MCPDispatchFast, session: "private-poll-session",
			occupant: mcpDispatchOccupantFor("trace", map[string]any{"operation": "call_chain"}),
		}
		ctx := withMCPDispatchQueueWait(withMCPDispatchRequest(t.Context(), info), 0)
		grant, err := pool.acquire(ctx, info.class, info.session)
		if grant != nil {
			grant.complete()
			t.Fatal("new fast call entered while both non-mutating slots were held")
		}
		if err == nil {
			t.Fatal("expected typed busy refusal")
		}
		return err
	}
	first := refuse()
	receipt.complete()
	acquire(MCPDispatchFast, "private-new-session", "customer/tool", "private-arguments")
	protocol.complete()
	second := refuse()

	check := func(err error, used int, expected map[string]int) {
		t.Helper()
		raw := MCPDispatchBusyResponse([]byte(`{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"trace","arguments":{"operation":"call_chain","target":"private-target"}}}`), err)
		var response struct {
			Error struct {
				Code int `json:"code"`
				Data struct {
					Outcome         string `json:"outcome"`
					DeliveryUnknown *bool  `json:"delivery_unknown"`
					QueueWaitMS     *int64 `json:"queue_wait_ms"`
					Occupancy       struct {
						Reason          string         `json:"reason"`
						Lane            string         `json:"lane"`
						UsedSlots       int            `json:"used_slots"`
						TotalSlots      int            `json:"total_slots"`
						RequestEligible bool           `json:"request_eligible"`
						InFlight        map[string]int `json:"in_flight"`
					} `json:"dispatch_occupancy"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		got := response.Error.Data
		if response.Error.Code != -32002 || got.Outcome != "server_busy" ||
			got.DeliveryUnknown == nil || *got.DeliveryUnknown || got.QueueWaitMS == nil || *got.QueueWaitMS != 0 {
			t.Fatalf("ordinary busy refusal changed: %s", raw)
		}
		if got.Occupancy.Reason != "queue_budget_unavailable" || got.Occupancy.Lane != "fast" ||
			got.Occupancy.TotalSlots != 4 || got.Occupancy.UsedSlots != used ||
			got.Occupancy.RequestEligible || len(got.Occupancy.InFlight) != len(expected) {
			t.Fatalf("wrong captured occupancy: %s", raw)
		}
		for name, count := range expected {
			if got.Occupancy.InFlight[name] != count {
				t.Fatalf("occupancy bucket %q = %d, want %d", name, got.Occupancy.InFlight[name], count)
			}
		}
		for _, private := range []string{"private-", "customer/tool"} {
			if strings.Contains(string(raw), private) {
				t.Fatalf("refusal disclosed a request identity: %s", raw)
			}
		}
	}
	// Render the first refusal after the live slots have changed.
	check(first, 4, map[string]int{"read.summary": 1, "change.receipt": 1, "edit": 1, "protocol": 1})
	check(second, 3, map[string]int{"read.summary": 1, "other": 1, "edit": 1})
}

func TestMCPDispatchBusyOccupancyDefaultOmitsObservation(t *testing.T) {
	coordinator := mcpDispatchCoordinator{}
	pool := coordinator.pool(newMCPDispatchSlots(1))
	holder, err := pool.acquire(t.Context(), MCPDispatchFast, "holder")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(holder.complete)
	ctx := withMCPDispatchQueueWait(t.Context(), 0)
	_, err = pool.acquire(ctx, MCPDispatchFast, "waiter")
	if err == nil {
		t.Fatal("expected typed busy")
	}
	raw := MCPDispatchBusyResponse([]byte(`{"id":1,"method":"tools/call"}`), err)
	if strings.Contains(string(raw), "dispatch_occupancy") || len(raw) == 0 {
		t.Fatalf("default busy wire changed: %s", raw)
	}
}
