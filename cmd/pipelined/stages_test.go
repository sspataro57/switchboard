package main

// SWT-40 Part E review fixes: PIPELINE_STAGES parsing, and the SWT-41
// "built is not deployed" guard — the image's one `go build` line must ship
// ./cmd/pipelined (cmd/orchestratord/dockerfile_test.go's shape).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/pipeline"
)

func TestParseStages(t *testing.T) {
	orig := stageImpls
	t.Cleanup(func() { stageImpls = orig })
	stageImpls = map[pipeline.Stage]stageImpl{
		pipeline.StageGate: {limit: 1, pass: func(*pgxpool.Pool) pipeline.PassFunc {
			return func(context.Context) (int, error) { return 0, nil }
		}},
	}

	for _, empty := range []string{"", " ", " , ,"} {
		got, err := parseStages(empty)
		if err != nil || len(got) != 0 {
			t.Errorf("parseStages(%q) = %v, %v; want no stages", empty, got, err)
		}
	}
	if got, err := parseStages(" gate "); err != nil || len(got) != 1 || got[0] != pipeline.StageGate {
		t.Errorf("parseStages(gate) = %v, %v", got, err)
	}
	for in, want := range map[string]string{
		"bogus":     "unknown stage",
		"gate,gate": "listed twice",
		"route":     "not implemented in this build",
		"Gate":      "unknown stage",
	} {
		if _, err := parseStages(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseStages(%q) error = %v, want one containing %q", in, err, want)
		}
	}
}

func TestDockerfile_BuildLineShipsPipelined(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\\\n", " ")
	var build string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "RUN ") && strings.Contains(l, "go build") {
			if build != "" {
				t.Fatalf("two `go build` RUN lines; this scan assumes one is the image's deploy list")
			}
			build = l
		}
	}
	fields := map[string]bool{}
	for _, f := range strings.Fields(build) {
		fields[f] = true
	}
	if !fields["./cmd/dashboard"] {
		t.Fatalf("POSITIVE CONTROL FAILED: the parsed build line does not list ./cmd/dashboard:\n%s", build)
	}
	if !fields["./cmd/pipelined"] && !fields["./cmd/..."] {
		t.Errorf("the image's build line does not ship ./cmd/pipelined: built is not deployed (SWT-41 landmine):\n%s", build)
	}
}
