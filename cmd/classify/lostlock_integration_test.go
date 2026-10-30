//go:build integration

package main

// swb 2907: pipelined shares the classify lock and holds it on every sweep, so
// classify-personal losing it is routine: it logs, exits 0, and the next tick
// picks the work up. Every other lane keeps the error.
//
// MUTATIONS THAT MUST TURN THIS RED:
//   - runCmd returns the error on the personal lane → the personal row;
//   - runCmd ignores the lost lock on the personal lane and runs anyway → the
//     personal row, which wants the skip line;
//   - runCmd returns nil on every lane → the residue, route and inquiry rows;
//   - promoteCmd swallows ErrLockHeld → the promote row.

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/promote"
)

// capturedStdout runs f and returns what it printed.
func capturedStdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	runErr := f()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(out), runErr
}

func TestLostLock_PersonalSkipsEveryOtherLaneRefuses(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping")
	}
	if strings.Contains(dsn, "192.168.50.49") {
		t.Fatal("refusing to run against the real ops db")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	for _, key := range []int64{classify.AdvisoryLockKey, promote.AdvisoryLockKey} {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
			t.Fatalf("lock %#x: %v", key, err)
		}
		if !ok {
			t.Fatalf("lock %#x is held by another test on this database", key)
		}
	}

	for _, tc := range []struct {
		name    string
		run     func([]string) error
		args    []string
		wantErr string
	}{
		{"run personal", runCmd, []string{"--lane", "personal"}, ""},
		{"run residue", runCmd, []string{"--lane", "residue", "--since", "72h"}, "holds the advisory lock"},
		{"run route", runCmd, []string{"--lane", "route", "--since", "720h"}, "holds the advisory lock"},
		{"run inquiry", runCmd, []string{"--lane", "inquiry", "--since", "336h"}, "holds the advisory lock"},
		{"promote personal", promoteCmd, []string{"--lane", "personal"}, "holds advisory lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := capturedStdout(t, func() error { return tc.run(tc.args) })
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil: classify-personal losing the lock is not a Failed Job", err)
				}
				if !strings.Contains(out, "skipping this run") {
					t.Fatalf("stdout = %q, want the skip line: a nil without it may have run with no lock", out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
