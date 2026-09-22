package main

// imap-idle-watch (SWT-73) criterion 19: the Dockerfile is UNCHANGED, because
// `./cmd/connectors/...` already builds this binary (Dockerfile:16-17) — so
// deployment/connector-google-watch runs /usr/local/bin/google with --watch and
// no image change is needed for the binary to exist.
//
// This is half of the SWT-41 landmine, asserted next to the binary the way
// cmd/orchestratord/dockerfile_test.go asserts its own: "A daemon a ticket
// depends on must be in the Dockerfile build line, have a manifest, and have a
// health signal judged from OUTSIDE the process, or the ticket is not
// delivered." orchestratord landed in SWT-5 and then was not in the image for
// two months. The other two halves are criterion 24's hand-off (the manifest)
// and criterion 6's /healthz.
//
// Plain unit test: reads ../../../Dockerfile, ZERO I/O beyond that.
//
// GREEN TODAY and must stay green — a criterion that forbids a change rather
// than requiring one. (Under a plain `go test ./cmd/connectors/google` the
// package compile-fails first on the greenfield symbols the other new test
// files name; run this file alone to see the assertion:
// `go test ./cmd/connectors/google/dockerfile_test.go`.)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfile_BuildLineAlreadyShipsTheGoogleConnector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\\\n", " ")

	var build string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "RUN ") && strings.Contains(l, "go build") {
			if build != "" {
				t.Fatalf("the Dockerfile has two `go build` RUN lines; this scan assumes ONE build line is "+
					"the image's deploy list:\n%s\n%s", build, l)
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
	if !fields["./cmd/dashboard"] {
		t.Fatalf("POSITIVE CONTROL FAILED: ./cmd/dashboard is not a token of the parsed build line, so the "+
			"parse is not seeing the package list:\n%s", build)
	}
	if !fields["./cmd/connectors/..."] && !fields["./cmd/connectors/google"] && !fields["./cmd/..."] {
		t.Errorf("the Dockerfile build line does not include ./cmd/connectors/..., so the image has no "+
			"/usr/local/bin/google and deployment/connector-google-watch has nothing to run (criterion 19, "+
			"and the SWT-41 landmine: built, never shipped):\n%s", build)
	}
}
