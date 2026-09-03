package main

import (
	"os"
	"testing"
)

func TestCommandPackageDisablesQueryTelemetry(t *testing.T) {
	if got := os.Getenv("GORTEX_QUERY_LOG_DISABLE"); got != "1" {
		t.Fatalf("GORTEX_QUERY_LOG_DISABLE = %q, want 1", got)
	}
}
