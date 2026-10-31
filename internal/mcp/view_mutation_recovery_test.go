package mcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/indexer"
)

func TestViewMutationRecoveryOnlyStaleNotBusyOrPending(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		allowed bool
	}{
		{"stale", indexer.ErrCheckoutMutationStale, true},
		{"disk", indexer.ErrCheckoutMutationDiskMoved, true},
		{"route", indexer.ErrCheckoutMutationRouteMoved, true},
		{"busy", indexer.ErrCheckoutMutationBusy, false},
		{"pending", indexer.ErrCheckoutMutationPending, false},
		{"joined_busy", errors.Join(indexer.ErrCheckoutMutationStale, indexer.ErrCheckoutMutationBusy), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prefix = "view_read_only: checkout mutation was not admitted; no files were changed"
			message := checkoutMutationRecoveryMessage(prefix, tc.err, tc.allowed)
			if !strings.HasPrefix(message, prefix) || strings.Contains(message, "Native read/edit is allowed") != tc.allowed {
				t.Fatalf("incorrect refusal qualification: %q", message)
			}
			if tc.allowed && (!strings.Contains(message, "retry the Gortex edit") || !strings.Contains(message, "reconnect the MCP session") || strings.Contains(strings.ToLower(message), "stop")) {
				t.Fatalf("missing usable recovery: %q", message)
			}
		})
	}
}

func TestViewMutationUnknownRecoveryMakesNoNativePermissionPromise(t *testing.T) {
	message := checkoutMutationRecoveryMessage("view_read_only: checkout mutation was not admitted", indexer.ErrCheckoutMutationStale, false)
	if strings.Contains(message, "Native read/edit is allowed") || !strings.Contains(message, "status was not established") || !strings.Contains(message, "retry the Gortex edit") || !strings.Contains(message, "reconnect the MCP session") {
		t.Fatalf("superseded/unknown publication promised permission: %s", message)
	}
}
