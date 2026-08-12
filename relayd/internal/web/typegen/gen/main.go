// Command gen writes console/src/api/types.ts from relayd's Go wire structs.
//
//	cd relayd && go generate ./internal/web/...
//
// It is a separate main package so that nothing in relayd's own build graph
// depends on it, and so `go build ./...` never needs the console checked out in
// any particular state.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/luthor007/relay/relayd/internal/web/typegen"
)

func main() {
	out := flag.String("o", "", "where to write (default: the checkout's console/src/api/types.ts)")
	check := flag.Bool("check", false, "exit non-zero if the file on disk is already stale, and write nothing")
	flag.Parse()

	if err := run(*out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(out string, check bool) error {
	want, err := typegen.Generate()
	if err != nil {
		return err
	}
	if out == "" {
		out, err = typegen.OutputFile()
		if err != nil {
			return err
		}
	}

	if check {
		got, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		if string(got) != string(want) {
			return fmt.Errorf("%s is stale — run: cd relayd && go generate ./internal/web/...", out)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, want, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "wrote", out)
	return nil
}
