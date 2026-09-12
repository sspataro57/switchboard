package main

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) criterion 1: the image's
// `go build` line ships ./cmd/orchestratord.
//
// This bug class is exactly "built, never shipped": orchestratord landed in
// SWT-5 (2026-07-11), ran once as a --once smoke, and then did not run for two
// months — its binary was not even in `switchboard:0.7.7`. The Dockerfile's
// build line IS the deploy list, so it is asserted here, next to the binary.
//
// Plain unit test: reads ../../Dockerfile, ZERO I/O beyond that.
//
// GREENFIELD NOTE — EXPECTED RED. Today the line is
// `./cmd/connectors/... ./cmd/tools/migrate ./cmd/dashboard ./cmd/google-auth
// ./cmd/classify`, so the orchestratord assertion fails. (This package's other
// test files reference symbols main.go does not define yet, so under a plain
// `go test ./cmd/orchestratord` the package compile-fails first; run this file
// alone with `go test ./cmd/orchestratord/dockerfile_test.go` to see the
// assertion.)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfile_BuildLineShipsOrchestratord(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	// Join shell line continuations so the multi-line RUN is one logical line.
	text := strings.ReplaceAll(string(raw), "\\\n", " ")

	var build string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "RUN ") && strings.Contains(l, "go build") {
			if build != "" {
				t.Fatalf("the Dockerfile has two `go build` RUN lines; this scan assumes ONE build line is the "+
					"image's deploy list:\n%s\n%s", build, l)
			}
			build = l
		}
	}
	if build == "" {
		t.Fatal("POSITIVE CONTROL FAILED: no `RUN ... go build` line found in the Dockerfile")
	}

	fields := map[string]bool{}
	for _, f := range strings.Fields(build) {
		fields[f] = true
	}
	// Positive control: the scan sees a package the image is known to ship.
	if !fields["./cmd/dashboard"] {
		t.Fatalf("POSITIVE CONTROL FAILED: ./cmd/dashboard is not a token of the parsed build line, so the "+
			"parse is not seeing the package list:\n%s", build)
	}
	// ./cmd/... would also ship it (and every other cmd); accepted as equivalent.
	if !fields["./cmd/orchestratord"] && !fields["./cmd/..."] {
		t.Errorf("the Dockerfile build line does not include ./cmd/orchestratord, so the image has no "+
			"/usr/local/bin/orchestratord and the Deployment cannot run it (criterion 1):\n%s", build)
	}
}
