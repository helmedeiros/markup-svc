// Package main is the markup-svc snapshot build tool. Reads a CSV
// rule file, builds an indexed engine, and writes the resulting
// markup.Snapshot as JSON. The output file is what cmd/markup-server
// --snapshot cold-starts from. See ADR-0007.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/snapshot"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-build: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable entry point. Errors prefix the offending stage
// (open rules, parse rules, build snapshot, write output) so a misuse
// fails with an actionable message rather than a stack trace.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("snapshot-build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rulesPath := fs.String("rules", "", "path to CSV rule file (required, see ADR-0002)")
	modelVersion := fs.String("model", "v1", "model version tag baked into the snapshot")
	outPath := fs.String("out", "", "path to write snapshot JSON (required, see ADR-0007)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rulesPath == "" {
		return fmt.Errorf("--rules is required")
	}
	if *outPath == "" {
		return fmt.Errorf("--out is required")
	}

	rules, err := readRules(*rulesPath)
	if err != nil {
		return err
	}
	snap, err := snapshot.Build(rules, *modelVersion)
	if err != nil {
		return fmt.Errorf("build snapshot: %w", err)
	}
	if err := writeSnapshot(*outPath, snap); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "snapshot-build: wrote %d rules (model=%s) to %s\n",
		len(rules), *modelVersion, *outPath)
	return nil
}

func readRules(path string) ([]load.Rule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rules %q: %w", path, err)
	}
	defer f.Close()
	rules, err := load.FromCSV(f)
	if err != nil {
		return nil, fmt.Errorf("parse rules %q: %w", path, err)
	}
	return rules, nil
}

func writeSnapshot(path string, snap *snapshot.Snapshot) error {
	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create output %q: %w", path, err)
	}
	defer out.Close()
	if err := snapshot.Write(out, snap); err != nil {
		return fmt.Errorf("write output %q: %w", path, err)
	}
	return nil
}
