package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(args []string) error {
	if len(args) > 0 && (args[0] == "run" || args[0] == "paired") {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if args[0] == "paired" {
			return pairedCommand(ctx, args[1:])
		}
		return runCommand(ctx, args[1:])
	}
	if (len(args) == 1 && args[0] == "schema") || (len(args) == 2 && args[0] == "schema" && args[1] == "paired") {
		schema := reportSchema()
		if len(args) == 2 {
			schema = pairedSchema()
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(schema)
	}
	if len(args) == 3 && args[0] == "delta" {
		before, err := readReport(args[1])
		if err != nil {
			return err
		}
		after, err := readReport(args[2])
		if err != nil {
			return err
		}
		return printDelta(os.Stdout, before, after)
	}
	return fmt.Errorf("usage: load run [options] | delta <before.json> <after.json> (received %d arguments)", len(args))
}
