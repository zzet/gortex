package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zzet/gortex/internal/citest"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root, err := os.Getwd()
	if err == nil {
		err = citest.RunSelect(ctx, root, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "CI test selection failed:", err)
		os.Exit(1)
	}
}
