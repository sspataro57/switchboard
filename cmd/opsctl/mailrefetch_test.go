package main

// U7 (SPEC mail-refetch-targeted / SWT-64, acceptance criteria 1, 2, 3, 11):
// `opsctl mail refetch` flag parsing. Offline, no pool, no IMAP — the
// gate_test.go / prreview_flags_test.go shape.
//
// The bounds are the safety property here. An unbounded run of a pass that
// overwrites raw_json in place, with no version history and no undo, is the
// thing criterion 2 exists to prevent; "it defaulted to everything" must not be
// reachable by forgetting a flag.
//
// GREENFIELD NOTE: cmd/opsctl/mailrefetch.go does not exist yet, so this file
// compile-FAILs with "undefined: parseMailRefetch" until it does — the expected
// failure mode. Imposed surface (SPEC "Files likely to touch" → cmd/opsctl
// holds flags, pool and printing only):
//
//	type mailRefetchOpts struct {
//	    query    google.RefetchQuery // From/Since/Until/RawIDs/Limit/AccountEmail
//	    maxBytes int                 // defaults to google.RefetchMaxMessageBytes
//	    dryRun   bool
//	}
//	func parseMailRefetch(argv []string) (mailRefetchOpts, error)
//
// --max-bytes defaults to RefetchMaxMessageBytes rather than to 0 so that an
// EXPLICIT `--max-bytes 0` is distinguishable from an omitted flag and can be
// refused (criterion 11: non-positive is an error, never a silent default — the
// zero-cap hazard MaxMessageBytes() guards against at imap.go:55-58).

import (
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

func TestParseMailRefetch_Bounds(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr []string // substrings the message must carry
	}{
		{
			name:    "no flags at all",
			argv:    nil,
			wantErr: []string{"--limit"},
		},
		{
			name:    "a selector but no limit",
			argv:    []string{"--from", "@placeholder.example"},
			wantErr: []string{"--limit is required", "unbounded"},
		},
		{
			name:    "raw ids but no limit",
			argv:    []string{"--raw-id", "73094"},
			wantErr: []string{"--limit is required", "unbounded"},
		},
		{
			name:    "limit zero",
			argv:    []string{"--from", "@placeholder.example", "--limit", "0"},
			wantErr: []string{"--limit"},
		},
		{
			name:    "limit negative",
			argv:    []string{"--from", "@placeholder.example", "--limit", "-1"},
			wantErr: []string{"--limit"},
		},
		{
			name:    "neither selector family",
			argv:    []string{"--limit", "50"},
			wantErr: []string{"--from", "--raw-id"},
		},
		{
			name:    "both selector families",
			argv:    []string{"--from", "@placeholder.example", "--raw-id", "73094", "--limit", "50"},
			wantErr: []string{"--from", "--raw-id"},
		},
		{
			name:    "since without a selector family",
			argv:    []string{"--since", "720h", "--limit", "50"},
			wantErr: []string{"--from", "--raw-id"},
		},
		{
			name:    "max-bytes zero",
			argv:    []string{"--raw-id", "73094", "--limit", "1", "--max-bytes", "0"},
			wantErr: []string{"--max-bytes"},
		},
		{
			name:    "max-bytes negative",
			argv:    []string{"--raw-id", "73094", "--limit", "1", "--max-bytes", "-1"},
			wantErr: []string{"--max-bytes"},
		},
		{
			name:    "unparseable since",
			argv:    []string{"--from", "@placeholder.example", "--since", "last tuesday", "--limit", "5"},
			wantErr: []string{"--since"},
		},
		{
			name:    "unparseable raw id",
			argv:    []string{"--raw-id", "73094,not-a-number", "--limit", "5"},
			wantErr: []string{"--raw-id"},
		},
		{
			name:    "unknown flag",
			argv:    []string{"--bogus"},
			wantErr: nil, // flag's own message; only the refusal matters
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseMailRefetch(c.argv)
			if err == nil {
				t.Fatalf("parseMailRefetch(%v) = nil error, want a refusal", c.argv)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("parseMailRefetch(%v) error = %q, want it to name %q", c.argv, err, want)
				}
			}
		})
	}
}

func TestParseMailRefetch_Accepts(t *testing.T) {
	t.Run("finder family", func(t *testing.T) {
		opts, err := parseMailRefetch([]string{"--from", "@placeholder.example", "--limit", "50"})
		if err != nil {
			t.Fatalf("parseMailRefetch: %v", err)
		}
		if opts.query.From != "@placeholder.example" {
			t.Errorf("query.From = %q, want %q", opts.query.From, "@placeholder.example")
		}
		if opts.query.Limit != 50 {
			t.Errorf("query.Limit = %d, want 50", opts.query.Limit)
		}
		if len(opts.query.RawIDs) != 0 {
			t.Errorf("query.RawIDs = %v, want empty", opts.query.RawIDs)
		}
		if opts.dryRun {
			t.Errorf("dryRun = true without --dry-run; a live run must be the explicit choice")
		}
		// Criterion 11: the tool's cap is its own number and does not read
		// MAIL_MAX_MESSAGE_BYTES.
		if opts.maxBytes != google.RefetchMaxMessageBytes {
			t.Errorf("maxBytes = %d, want RefetchMaxMessageBytes = %d", opts.maxBytes, google.RefetchMaxMessageBytes)
		}
	})

	t.Run("explicit family, several ids", func(t *testing.T) {
		opts, err := parseMailRefetch([]string{"--raw-id", "73094,73080", "--limit", "2", "--dry-run"})
		if err != nil {
			t.Fatalf("parseMailRefetch: %v", err)
		}
		want := []int64{73094, 73080}
		if len(opts.query.RawIDs) != len(want) {
			t.Fatalf("query.RawIDs = %v, want %v", opts.query.RawIDs, want)
		}
		for i := range want {
			if opts.query.RawIDs[i] != want[i] {
				t.Errorf("query.RawIDs = %v, want %v", opts.query.RawIDs, want)
				break
			}
		}
		if opts.query.From != "" {
			t.Errorf("query.From = %q, want empty on the explicit family", opts.query.From)
		}
		if !opts.dryRun {
			t.Errorf("dryRun = false with --dry-run")
		}
	})

	t.Run("since accepts a Go duration", func(t *testing.T) {
		before := time.Now()
		opts, err := parseMailRefetch([]string{"--from", "@placeholder.example", "--since", "720h", "--limit", "50"})
		if err != nil {
			t.Fatalf("parseMailRefetch: %v", err)
		}
		if opts.query.Since.IsZero() {
			t.Fatalf("query.Since is zero after --since 720h")
		}
		want := before.Add(-720 * time.Hour)
		if d := opts.query.Since.Sub(want); d < -time.Minute || d > time.Minute {
			t.Errorf("query.Since = %s, want ~%s (now - 720h)", opts.query.Since, want)
		}
	})

	t.Run("since and until accept RFC3339", func(t *testing.T) {
		opts, err := parseMailRefetch([]string{
			"--from", "@placeholder.example",
			"--since", "2026-09-01T00:00:00Z",
			"--until", "2026-09-10T00:00:00Z",
			"--limit", "50",
		})
		if err != nil {
			t.Fatalf("parseMailRefetch: %v", err)
		}
		wantSince := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		wantUntil := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
		if !opts.query.Since.Equal(wantSince) {
			t.Errorf("query.Since = %s, want %s", opts.query.Since, wantSince)
		}
		if !opts.query.Until.Equal(wantUntil) {
			t.Errorf("query.Until = %s, want %s", opts.query.Until, wantUntil)
		}
	})

	t.Run("account narrows the selection", func(t *testing.T) {
		opts, err := parseMailRefetch([]string{
			"--from", "@placeholder.example", "--limit", "5",
			"--account", "placeholder-account@placeholder.example",
			"--max-bytes", "5242880",
		})
		if err != nil {
			t.Fatalf("parseMailRefetch: %v", err)
		}
		if opts.query.AccountEmail != "placeholder-account@placeholder.example" {
			t.Errorf("query.AccountEmail = %q", opts.query.AccountEmail)
		}
		if opts.maxBytes != 5242880 {
			t.Errorf("maxBytes = %d, want 5242880 (--max-bytes override)", opts.maxBytes)
		}
	})
}

// groupTargetsByAccount groups by ACCOUNT ID, not by email — the case-variant
// hazard review found. source_accounts is unique only on
// (provider, account_email), so two rows differing only in case are legal, while
// ListAppPasswordAccounts matches lower(account_email). Grouping by the string
// would hand one batch to whichever row sorted first, and the pass would then
// refuse every target in it as wrong_account: correct, but useless.
func TestGroupTargetsByAccount(t *testing.T) {
	mk := func(rawID, acctID int64, email string) google.RefetchTarget {
		return google.RefetchTarget{RawID: rawID, AccountID: acctID, AccountEmail: email}
	}

	t.Run("empty", func(t *testing.T) {
		if got := groupTargetsByAccount(nil); len(got) != 0 {
			t.Errorf("groupTargetsByAccount(nil) = %+v, want empty", got)
		}
	})

	t.Run("one account keeps its targets in order", func(t *testing.T) {
		got := groupTargetsByAccount([]google.RefetchTarget{
			mk(3, 77, "a@placeholder.example"),
			mk(1, 77, "a@placeholder.example"),
		})
		if len(got) != 1 {
			t.Fatalf("batches = %d, want 1 (%+v)", len(got), got)
		}
		if got[0].accountID != 77 || got[0].email != "a@placeholder.example" {
			t.Errorf("batch = %+v, want account 77 a@placeholder.example", got[0])
		}
		if len(got[0].targets) != 2 || got[0].targets[0].RawID != 3 || got[0].targets[1].RawID != 1 {
			t.Errorf("targets = %+v, want raw ids [3 1] in selection order", got[0].targets)
		}
	})

	t.Run("interleaved accounts split, sorted by id", func(t *testing.T) {
		got := groupTargetsByAccount([]google.RefetchTarget{
			mk(1, 90, "b@placeholder.example"),
			mk(2, 77, "a@placeholder.example"),
			mk(3, 90, "b@placeholder.example"),
		})
		if len(got) != 2 {
			t.Fatalf("batches = %d, want 2 (%+v)", len(got), got)
		}
		if got[0].accountID != 77 || got[1].accountID != 90 {
			t.Errorf("batch order = %d, %d; want 77 then 90 (sorted, so a run is deterministic)",
				got[0].accountID, got[1].accountID)
		}
		if len(got[1].targets) != 2 {
			t.Errorf("account 90 got %d targets, want 2", len(got[1].targets))
		}
	})

	// The one that matters: same spelling, different rows.
	t.Run("same email different ids do not merge", func(t *testing.T) {
		got := groupTargetsByAccount([]google.RefetchTarget{
			mk(1, 77, "Salvador@placeholder.example"),
			mk(2, 78, "salvador@placeholder.example"),
		})
		if len(got) != 2 {
			t.Fatalf("batches = %d, want 2: case-distinct source_accounts rows are legal and must not be "+
				"merged onto one account id (%+v)", len(got), got)
		}
		if got[0].accountID == got[1].accountID {
			t.Errorf("both batches carry account %d", got[0].accountID)
		}
	})
}
