package dashboard

// task-detail-source-message (SWT-65) test plan section B: criterion 18, the
// pure channel-aware heading. No db, no pgx, no model — the sections_test.go /
// lights_test.go shape (SWT-52's "the light is Go, not template": the TEMPLATE
// never branches on the channel).
//
// IMPOSED SURFACE (D7):
//
//	func sourceMessageHeading(channel string, viaThread bool) string
//
// GREENFIELD NOTE — EXPECTED RED: sourcemessage.go does not exist, so package
// dashboard's test binary compile-FAILS here with `undefined:
// sourceMessageHeading`. That is this file's expected failure mode; section A
// (task_detail_structure_test.go) is written to reference no new identifier so
// its assertions stay readable once this one compiles.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSourceMessageHeading_Table(t *testing.T) {
	// D7's table, all ten strings. viaThread is branch 3 (tasks.source_thread_id
	// picked the message): that branch's wording must NOT overclaim, because the
	// database never said that message raised the task.
	cases := []struct {
		channel   string
		viaThread bool
		want      string
	}{
		{"gmail", false, "Source email"},
		{"gmail", true, "Latest email on the source thread"},
		{"slack", false, "Source Slack message"},
		{"slack", true, "Latest Slack message on the source thread"},
		{"upwork", false, "Source Upwork message"},
		{"upwork", true, "Latest Upwork message on the source thread"},
		{"jira", false, "Source Jira comment"},
		{"jira", true, "Latest Jira comment on the source thread"},
		// Anything else / empty → the generic pair, never a panic, never empty.
		{"", false, "Source message"},
		{"", true, "Latest message on the source thread"},
		{"mastodon", false, "Source message"},
		{"mastodon", true, "Latest message on the source thread"},
	}
	for _, c := range cases {
		got := sourceMessageHeading(c.channel, c.viaThread)
		if got != c.want {
			t.Errorf("sourceMessageHeading(%q, %v) = %q, want %q (criterion 18 / D7)", c.channel, c.viaThread, got, c.want)
		}
	}
}

// An unknown channel must not collapse the two branches into one string: the
// generic pair still distinguishes "this message made this task" from "this is
// the latest message on a conversation the task names".
func TestSourceMessageHeading_UnknownChannelKeepsTheBranchDistinction(t *testing.T) {
	for _, ch := range []string{"", "mastodon", "GMAIL_BUT_SHOUTED", "gmail "} {
		a, b := sourceMessageHeading(ch, false), sourceMessageHeading(ch, true)
		if a == "" || b == "" {
			t.Errorf("sourceMessageHeading(%q, …) returned an empty heading (%q, %q): criterion 18 says never a "+
				"panic or an empty heading", ch, a, b)
		}
		if a == b {
			t.Errorf("sourceMessageHeading(%q, false) == sourceMessageHeading(%q, true) == %q: branch 3's wording "+
				"is not cosmetic (D7)", ch, ch, a)
		}
	}
}

// A broken read and an absent message are different facts, and the page draws
// the same blank for both. loadSourceMessage must at least tell its caller
// which one happened: if a renamed column made the statement fail, the section
// would disappear from EVERY task page and look exactly like the 33-of-51
// majority that legitimately has no source message.
//
// No database and no network: the DSN names a unix socket directory that does
// not exist, so the pool fails to connect on first use and the error is
// deterministic — and it is not pgx.ErrNoRows, which is the whole point.
func TestLoadSourceMessage_ABrokenReadIsNotAnAbsentMessage(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres:///ops?host=/nonexistent-switchboard-socket-dir")
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	defer pool.Close()

	s := &Server{pool: pool}
	sm, err := s.loadSourceMessage(ctx, 1, nil)
	if sm != nil {
		t.Errorf("loadSourceMessage returned a section (%+v) from a pool that cannot connect", sm)
	}
	if err == nil {
		t.Fatal("loadSourceMessage swallowed a connection failure as (nil, nil): absent-because-none and " +
			"absent-because-impossible must not be the same value")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a connection failure was reported as pgx.ErrNoRows: %v", err)
	}
	if !strings.Contains(err.Error(), "resolve source message") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}
