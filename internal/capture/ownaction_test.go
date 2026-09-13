package capture

// SWT-45 J17, the own-action guard's pure half (ownaction.go): the window's
// edges, the freshness instant, the From parse and the verdict table, plus
// RulesConfig's live-horizon floor. ZERO I/O. The SQL binds ownActionWindow's
// two values, so these edges are the SQL's edges too
// (ownaction_integration_test.go proves the predicate bites).

import (
	"strings"
	"testing"
	"time"
)

func TestOwnActionWindow_EdgesAreInclusive(t *testing.T) {
	email := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	from, to := ownActionWindow(email)
	in := func(outbound time.Time) bool { return !outbound.Before(from) && !outbound.After(to) }

	for _, tc := range []struct {
		name     string
		outbound time.Time
		want     bool
	}{
		{"exactly OwnActionLead before the email", email.Add(-OwnActionLead), true},
		{"one nanosecond earlier than that", email.Add(-OwnActionLead - time.Nanosecond), false},
		{"the email's own instant", email, true},
		{"exactly OwnActionLag after the email", email.Add(OwnActionLag), true},
		{"one nanosecond later than that", email.Add(OwnActionLag + time.Nanosecond), false},
		{"an hour before (an old comment of his)", email.Add(-time.Hour), false},
	} {
		if got := in(tc.outbound); got != tc.want {
			t.Errorf("%s: in window = %v, want %v (window [%s, %s])", tc.name, got, tc.want, from, to)
		}
	}
	// The measured prod shape the constants encode: the 26 "Anonymous (JIRA)"
	// emails that correlate with a comment of his (of 208 Anonymous Treetop
	// emails; the rest are edit notices the guard does not cover) all had his
	// outbound message in [email-10m, email+2m]. A change here is a decision.
	if OwnActionLead != 10*time.Minute || OwnActionLag != 2*time.Minute {
		t.Errorf("window is [-%s, +%s], want [-10m, +2m] (the prod correlation, SPEC J17)", OwnActionLead, OwnActionLag)
	}
}

func TestOwnActionSyncedPast_IsPastTheWindowsFarEdge(t *testing.T) {
	email := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	_, to := ownActionWindow(email)
	if got := ownActionSyncedPast(email); !got.After(to) {
		t.Errorf("ownActionSyncedPast = %s, want strictly after the window's far edge %s: a poller run that "+
			"started inside the window may not have seen a comment written at its end", got, to)
	}
}

// The From parse. The stored sender for IMAP mail is the decoded raw From
// header, so prod carries the QUOTED form; the unquoted form is the same
// header written without quotes.
func TestJiraNotificationActor_Parse(t *testing.T) {
	for _, tc := range []struct {
		from      string
		wantName  string
		wantOK    bool
		wantNamed string // namedJiraActor
	}{
		{`"Katie Evans (JIRA)" <jira@treetopllc.jira.com>`, "Katie Evans", true, "Katie Evans"},
		{`Katie Evans (JIRA) <jira@treetopllc.jira.com>`, "Katie Evans", true, "Katie Evans"},
		{`  "Katie Evans (JIRA)"   <jira@treetopllc.jira.com>  `, "Katie Evans", true, "Katie Evans"},
		{`Katie Evans (JIRA)`, "Katie Evans", true, "Katie Evans"},
		{`"Anonymous (JIRA)" <jira@treetopllc.jira.com>`, "Anonymous", true, ""},
		{`Anonymous (JIRA) <jira@treetopllc.jira.com>`, "Anonymous", true, ""},
		{`"anonymous  (JIRA)" <jira@treetopllc.jira.com>`, "anonymous", true, ""},
		{`Jira <jira@treetopllc.jira.com>`, "", false, ""},
		{`"Jira" <jira@treetopllc.jira.com>`, "", false, ""},
		{`jira@treetopllc.jira.com`, "", false, ""},
		{`<jira@treetopllc.jira.com>`, "", false, ""},
		{``, "", false, ""},
		{`"(JIRA)" <jira@treetopllc.jira.com>`, "", false, ""},
		{`"Katie (QA) Evans (JIRA)" <jira@treetopllc.jira.com>`, "Katie (QA) Evans", true, "Katie (QA) Evans"},
		{`Bob Smith (contractor) (JIRA) <jira@treetopllc.jira.com>`, "Bob Smith (contractor)", true, "Bob Smith (contractor)"},
		{`"Dana \"DJ\" Jones (JIRA)" <jira@treetopllc.jira.com>`, `Dana "DJ" Jones`, true, `Dana "DJ" Jones`},
		{`"Katie Evans (JIRA) via Treetop" <jira@treetopllc.jira.com>`, "", false, ""},
		{`Katie Evans <katie@treetopllc.com>`, "", false, ""},
	} {
		name, ok := jiraNotificationActor(tc.from)
		if name != tc.wantName || ok != tc.wantOK {
			t.Errorf("jiraNotificationActor(%q) = (%q, %v), want (%q, %v)", tc.from, name, ok, tc.wantName, tc.wantOK)
		}
		if got := namedJiraActor(tc.from); got != tc.wantNamed {
			t.Errorf("namedJiraActor(%q) = %q, want %q", tc.from, got, tc.wantNamed)
		}
	}
}

func TestSameJiraActor(t *testing.T) {
	if !sameJiraActor("Salvador Spataro", " salvador   SPATARO ") {
		t.Errorf("case and whitespace must not make a different person")
	}
	if sameJiraActor("Katie Evans", "Salvador Spataro") {
		t.Errorf("two different names compared equal")
	}
}

func TestDecideOwnAction_Table(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  ownActionObservation
		want string
	}{
		{"no poller covers the key", ownActionObservation{pollers: 0, found: false, fresh: false}, ownActionNotApplicable},
		{"no poller, even with a stale clock long past the bound", ownActionObservation{waited: 2 * OwnActionMaxWait}, ownActionNotApplicable},
		{"no poller, named actor", ownActionObservation{actor: "Katie Evans"}, ownActionNotApplicable},
		{"found on a fresh thread", ownActionObservation{pollers: 1, found: true, fresh: true}, ownActionSkip},
		{"found on a STALE thread still skips", ownActionObservation{pollers: 1, found: true, fresh: false}, ownActionSkip},
		{"fresh, nothing of his", ownActionObservation{pollers: 1, fresh: true}, ownActionClear},
		{"stale, just ingested", ownActionObservation{pollers: 1}, ownActionDefer},
		{"stale, one ns inside the bound", ownActionObservation{pollers: 1, waited: OwnActionMaxWait - time.Nanosecond}, ownActionDefer},
		{"stale, exactly at the bound", ownActionObservation{pollers: 1, waited: OwnActionMaxWait}, ownActionBlind},
		{"stale, past the bound", ownActionObservation{pollers: 2, waited: time.Hour}, ownActionBlind},
		// The named-actor exemption: evaluated before the window verdict and
		// before freshness, so it neither skips nor waits.
		{"named other, his comment in the window (WEB-10355)",
			ownActionObservation{pollers: 1, actor: "Katie Evans", found: true, hisName: "Salvador Spataro"}, ownActionNamed},
		{"named other, nothing in the window, stale thread",
			ownActionObservation{pollers: 1, actor: "Katie Evans"}, ownActionNamed},
		{"named other, stale past the bound: still named, not blind",
			ownActionObservation{pollers: 1, actor: "Katie Evans", waited: time.Hour}, ownActionNamed},
		{"named as HIM, his comment in the window",
			ownActionObservation{pollers: 1, actor: "salvador spataro", found: true, hisName: "Salvador Spataro"}, ownActionSkip},
		{"named as him, nothing in the window: cannot tell, so named (never observed on prod)",
			ownActionObservation{pollers: 1, actor: "Salvador Spataro"}, ownActionNamed},
	} {
		if got := decideOwnAction(tc.obs); got != tc.want {
			t.Errorf("%s: decideOwnAction(%+v) = %q, want %q", tc.name, tc.obs, got, tc.want)
		}
	}
}

// ownActionSettled is what lets the store skip the freshness reads: it must
// settle every named-other case, and leave anonymous/no-shape cases with
// nothing in the window to freshness.
func TestOwnActionSettled_OnlyFreshnessCasesAreOpen(t *testing.T) {
	if _, ok := ownActionSettled(ownActionObservation{pollers: 1, actor: "Katie Evans"}); !ok {
		t.Errorf("a named-other actor must settle without freshness (no wait, no freshness reads)")
	}
	if _, ok := ownActionSettled(ownActionObservation{pollers: 1}); ok {
		t.Errorf("an anonymous email with nothing in the window must go to freshness")
	}
}

// ---- the live-horizon floor (second review batch) ----------------------------

func TestRulesConfig_LiveHorizonFloor(t *testing.T) {
	if MinLiveRulesHorizon < OwnActionMaxWait+15*time.Minute {
		t.Fatalf("MinLiveRulesHorizon %s does not even cover OwnActionMaxWait %s plus one */15 pass",
			MinLiveRulesHorizon, OwnActionMaxWait)
	}
	for _, tc := range []struct {
		name    string
		cfg     RulesConfig
		wantErr bool
		wantH   time.Duration
	}{
		{"live, one ns under the floor", RulesConfig{Mode: RulesModeLive, Horizon: MinLiveRulesHorizon - time.Nanosecond}, true, 0},
		{"live, 30m", RulesConfig{Mode: RulesModeLive, Horizon: 30 * time.Minute}, true, 0},
		{"live, exactly the floor", RulesConfig{Mode: RulesModeLive, Horizon: MinLiveRulesHorizon}, false, MinLiveRulesHorizon},
		{"live, unset takes the default", RulesConfig{Mode: RulesModeLive}, false, DefaultRulesHorizon},
		{"shadow, 10m is not floored", RulesConfig{Mode: RulesModeShadow, Horizon: 10 * time.Minute}, false, 10 * time.Minute},
		{"shadow, unbounded", RulesConfig{}, false, 0},
	} {
		got, err := tc.cfg.normalize()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: normalize err = %v, want error %v", tc.name, err, tc.wantErr)
			continue
		}
		if err != nil {
			if !strings.Contains(err.Error(), MinLiveRulesHorizon.String()) {
				t.Errorf("%s: error %q does not state the floor %s", tc.name, err, MinLiveRulesHorizon)
			}
			continue
		}
		if got.Horizon != tc.wantH {
			t.Errorf("%s: horizon = %s, want %s", tc.name, got.Horizon, tc.wantH)
		}
	}
}

// CAPTURE_RULES_SINCE: garbage and non-positive values still fall back silently
// (existing behaviour, unchanged); a too-small POSITIVE value passes through
// RulesHorizon and now errors the live pass at normalize.
func TestRulesHorizon_EnvThroughTheFloor(t *testing.T) {
	for _, tc := range []struct {
		env     string
		wantErr bool
		wantH   time.Duration
	}{
		{"30m", true, 0},
		{"1h59m", true, 0},
		{"2h", false, 2 * time.Hour},
		{"720", false, DefaultRulesHorizon}, // not a Go duration: silent fallback
		{"-5h", false, DefaultRulesHorizon}, // non-positive: silent fallback
		{"", false, DefaultRulesHorizon},
	} {
		t.Setenv("CAPTURE_RULES_SINCE", tc.env)
		cfg, err := RulesConfig{Mode: RulesModeLive, Horizon: RulesHorizon(RulesModeLive)}.normalize()
		if (err != nil) != tc.wantErr {
			t.Errorf("CAPTURE_RULES_SINCE=%q: err = %v, want error %v", tc.env, err, tc.wantErr)
			continue
		}
		if err == nil && cfg.Horizon != tc.wantH {
			t.Errorf("CAPTURE_RULES_SINCE=%q: horizon = %s, want %s", tc.env, cfg.Horizon, tc.wantH)
		}
	}
}
