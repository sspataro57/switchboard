package swbpush

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func always(string) bool { return true }

func cfg() Config {
	return Config{Windows: map[string][]string{"collab": {"collaboratory", "a-millon"}, "sbc": {"switchboard"}}}
}

func sess(win, turn string, idleFor time.Duration) Session {
	return Session{SID: "s-" + win, Pane: "%" + win, Window: win, LiveWindow: win, LiveCmd: "claude",
		Turn: turn, TurnAt: t0.Add(-idleFor)}
}

// primed is a Seen where every mapped queue was already looked at, holding old.
func primed(old ...Task) Seen {
	s := Seen{Key("collab", "collaboratory"): {}, Key("collab", "a-millon"): {}, Key("sbc", "switchboard"): {}}
	for _, t := range old {
		for _, w := range []string{"collab", "sbc"} {
			if m := s[Key(w, t.Slug)]; m != nil {
				m[t.ID] = t.At
			}
		}
	}
	return s
}

func TestPlan_FirstSightRecordsEverythingAndSendsNothing(t *testing.T) {
	seen := Seen{}
	tasks := []Task{{1, "collaboratory", "", t0.Add(-time.Hour), "", false}}
	if n := Plan(cfg(), []Session{sess("collab", "idle", time.Hour)}, tasks, seen, t0, always); len(n) != 0 {
		t.Fatalf("first sight nudged: %+v", n)
	}
	if !seen[Key("collab", "collaboratory")][1].Equal(t0.Add(-time.Hour)) {
		t.Errorf("task 1 not recorded: %v", seen)
	}
	if seen[Key("collab", "a-millon")] == nil {
		t.Errorf("an empty queue is marked seen too, so its first task nudges")
	}
}

func TestPlan_NewTaskNudgesIdleAndBusy_AndSettlesOnlyOnApply(t *testing.T) {
	for _, turn := range []string{"idle", "busy"} {
		seen := primed(Task{1, "collaboratory", "", t0.Add(-time.Hour), "", false})
		tasks := []Task{{1, "collaboratory", "", t0.Add(-time.Hour), "", false}, {2, "collaboratory", "", t0.Add(-time.Minute), "", false}}
		n := Plan(cfg(), []Session{sess("collab", turn, time.Hour)}, tasks, seen, t0, always)
		if len(n) != 1 || n[0].Pane != "%collab" || !strings.Contains(n[0].Text, "collaboratory queue") {
			t.Fatalf("%s: nudges %+v, want one for collaboratory", turn, n)
		}
		if n[0].Typed != (turn == "idle") {
			t.Errorf("%s: Typed=%v; only an idle session is typed into, a busy one goes to the hook", turn, n[0].Typed)
		}
		if _, ok := seen[Key("collab", "collaboratory")][2]; ok {
			t.Errorf("%s: Plan settled the new task itself; only Apply after submitting may", turn)
		}
		if again := Plan(cfg(), []Session{sess("collab", turn, time.Hour)}, tasks, seen, t0, always); len(again) != 1 {
			t.Errorf("%s: an unsubmitted nudge is not retried", turn)
		}
		seen.Apply(n[0].Settle)
		if again := Plan(cfg(), []Session{sess("collab", turn, time.Hour)}, tasks, seen, t0, always); len(again) != 0 {
			t.Errorf("%s: nudged twice for the same task: %+v", turn, again)
		}
	}
}

func TestPlan_LateCommitStillCounts(t *testing.T) {
	// Task 3's now() is OLDER than task 2's, but it committed after task 2 was
	// seen: a single high-water mark would miss it forever.
	seen := primed(Task{2, "collaboratory", "", t0, "", false})
	tasks := []Task{{2, "collaboratory", "", t0, "", false}, {3, "collaboratory", "", t0.Add(-time.Second), "", false}}
	if n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, tasks, seen, t0.Add(time.Minute), always); len(n) != 1 {
		t.Fatalf("late-committed task not noticed: %+v", n)
	}
}

func TestPlan_ActivityOnAKnownTaskCounts(t *testing.T) {
	seen := primed(Task{1, "collaboratory", "", t0.Add(-time.Hour), "", false})
	tasks := []Task{{1, "collaboratory", "", t0.Add(-time.Minute), "", false}}
	if n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, tasks, seen, t0, always); len(n) != 1 {
		t.Fatalf("new activity on a known task not noticed: %+v", n)
	}
}

func TestPlan_Gates(t *testing.T) {
	tasks := []Task{{2, "collaboratory", "", t0.Add(-time.Minute), "", false}}
	for _, tc := range []struct {
		name     string
		ss       []Session
		composer bool
	}{
		{"permission prompt", []Session{sess("collab", "prompt", time.Hour)}, true},
		{"gone", []Session{sess("collab", "gone", time.Hour)}, true},
		{"idle too briefly", []Session{sess("collab", "idle", 5*time.Second)}, true},
		{"composer not empty (idle)", []Session{sess("collab", "idle", time.Hour)}, false},
		{"not claude", []Session{func() Session { s := sess("collab", "idle", time.Hour); s.LiveCmd = "bash"; return s }()}, true},
		{"window renamed", []Session{func() Session { s := sess("collab", "idle", time.Hour); s.Window = "old"; return s }()}, true},
		{"unmapped window", []Session{sess("kube", "idle", time.Hour)}, true},
		{"two sessions in one window", []Session{sess("collab", "idle", time.Hour),
			func() Session { s := sess("collab", "busy", 0); s.Pane = "%9"; return s }()}, true},
	} {
		if n := Plan(cfg(), tc.ss, tasks, primed(), t0, func(string) bool { return tc.composer }); len(n) != 0 {
			t.Errorf("%s: nudged %+v", tc.name, n)
		}
	}
}

func TestPlan_BusySessionIgnoresTheComposer(t *testing.T) {
	n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, []Task{{2, "collaboratory", "", t0.Add(-time.Minute), "", false}}, primed(), t0,
		func(string) bool { t.Fatal("a busy session's screen was read"); return false })
	if len(n) != 1 || n[0].Typed {
		t.Fatalf("busy: %+v, want one hook nudge", n)
	}
}

func TestPlan_DeadPaneDoesNotFreezeTheWindow(t *testing.T) {
	dead := sess("collab", "idle", time.Hour)
	dead.Pane, dead.LiveCmd = "%9", "bash"
	n := Plan(cfg(), []Session{sess("collab", "busy", 0), dead}, []Task{{2, "collaboratory", "", t0.Add(-time.Minute), "", false}}, primed(), t0, always)
	if len(n) != 1 || n[0].Pane != "%collab" {
		t.Fatalf("a leftover non-claude pane blocked the window: %+v", n)
	}
}

func TestPlan_OwnTasksNeverNudge_EvenWhenReleased(t *testing.T) {
	seen := primed()
	mine := []Task{{5, "switchboard", "sbc", t0.Add(-time.Minute), "", false}}
	if n := Plan(cfg(), []Session{sess("sbc", "idle", time.Hour)}, mine, seen, t0, always); len(n) != 0 {
		t.Fatalf("nudged about its own task: %+v", n)
	}
	released := []Task{{5, "switchboard", "", t0.Add(-time.Minute), "", false}}
	if n := Plan(cfg(), []Session{sess("sbc", "idle", time.Hour)}, released, seen, t0, always); len(n) != 0 {
		t.Errorf("releasing its own task nudged the session: %+v", n)
	}
	other := append(released, Task{6, "switchboard", "collab", t0.Add(-time.Minute), "", false})
	if n := Plan(cfg(), []Session{sess("sbc", "idle", time.Hour)}, other, seen, t0, always); len(n) != 1 {
		t.Errorf("another session's task is news for this one: %+v", n)
	}
}

func TestPlan_PrunesTasksOutOfPlay(t *testing.T) {
	seen := primed(Task{1, "collaboratory", "", t0.Add(-time.Minute), "", false})
	Plan(cfg(), []Session{sess("collab", "busy", 0)}, nil, seen, t0, always)
	if _, ok := seen[Key("collab", "collaboratory")][1]; ok {
		t.Errorf("a task out of play stays in memory")
	}
}

func TestPlan_TwoQueuesOneLine(t *testing.T) {
	tasks := []Task{{1, "collaboratory", "", t0.Add(-time.Minute), "", false}, {2, "a-millon", "", t0.Add(-time.Minute), "", false}}
	n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, tasks, primed(), t0, always)
	if len(n) != 1 || !strings.Contains(n[0].Text, "a-millon, collaboratory queue") || len(n[0].Settle) != 2 {
		t.Fatalf("got %+v", n)
	}
}

var nbsp = string(rune(0x00A0))

func box(lines ...string) string {
	rule := "\x1b[38;5;244m" + strings.Repeat("─", 40) + "\x1b[39m"
	return "output\n" + rule + "\n" + strings.Join(lines, "\n") + "\n" + rule + "\n  ⏵⏵ bypass permissions on\n"
}

func TestComposer(t *testing.T) {
	marker := "❯" + nbsp
	for _, tc := range []struct {
		name, capture string
		empty         bool
	}{
		{"empty", box("\x1b[38;5;246m" + marker + "\x1b[39m"), true},
		{"dim suggestion", box("\x1b[39m" + marker + "\x1b[2mcheck comms, prs\x1b[0m"), true},
		{"typed", box("\x1b[39m" + marker + "half a sentence"), false},
		{"draft on line 2 (shift-enter)", box(marker, "  second line of a draft"), false},
		{"permission dialog (no rule-bounded box)", "Do you want to proceed?\n" + marker + "1. Yes\n  2. No\n", false},
		{"dialog under a rule, no bottom rule", strings.Repeat("─", 40) + "\n" + marker + "1. Yes\n  2. No\n", false},
		{"nothing", "", false},
	} {
		if got := ComposerEmpty(tc.capture); got != tc.empty {
			t.Errorf("%s: ComposerEmpty = %v, want %v", tc.name, got, tc.empty)
		}
	}
}

func TestComposerHolds(t *testing.T) {
	marker := "❯" + nbsp
	text := Text([]string{"collaboratory"})
	if !ComposerHolds(box(marker+text), text) {
		t.Errorf("exact nudge not recognised")
	}
	if !ComposerHolds(box(marker+text[:30], "  "+text[30:]), text) {
		t.Errorf("wrapped nudge not recognised")
	}
	if ComposerHolds(box(marker+"his words "+text), text) {
		t.Errorf("nudge mixed with his typing counted as the nudge")
	}
	if ComposerHolds("Do you want to proceed?\n"+marker+"1. Yes\n", text) {
		t.Errorf("a dialog counted as holding the nudge")
	}
}

func TestUnattended(t *testing.T) {
	projects := []string{"collaboratory", "a-millon", "town-ai", "smoke"}
	c := cfg()
	c.NotifySkip = []string{"smoke"}
	collab := sess("collab", "busy", 0)
	seen := Seen{}
	// First pass: everything already there is first sight.
	old := []Task{{1, "collaboratory", "", t0.Add(-time.Hour), "old", false}}
	if got, _ := Unattended(c, []Session{collab}, projects, old, seen, t0.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("first sight emailed: %+v", got)
	}
	tasks := append(old,
		Task{2, "town-ai", "", t0, "Erica Rapa: login bug", false},   // first task of an empty project
		Task{3, "collaboratory", "", t0, "watched by collab", false}, // a console has it
		Task{4, "smoke", "", t0, "muted", false},                     // notify_skip
	)
	got, settle := Unattended(c, []Session{collab}, projects, tasks, seen, t0.Add(time.Hour))
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("got %+v, want only town-ai's #2", got)
	}
	if again, _ := Unattended(c, []Session{collab}, projects, tasks, seen, t0.Add(time.Hour)); len(again) != 1 {
		t.Errorf("an unsent email is not retried")
	}
	seen.Apply(settle)
	if again, _ := Unattended(c, []Session{collab}, projects, tasks, seen, t0.Add(time.Hour)); len(again) != 0 {
		t.Errorf("emailed twice: %+v", again)
	}
	// The collab console goes away: collaboratory's NEW work now emails.
	tasks = append(tasks, Task{5, "collaboratory", "", t0.Add(time.Minute), "nobody watching", false})
	if got, _ := Unattended(c, nil, projects, tasks, seen, t0.Add(time.Hour)); len(got) != 1 || got[0].ID != 5 {
		t.Errorf("with no live console, got %+v, want #5", got)
	}
	subj, body := Mail([]Task{tasks[1]})
	if !strings.Contains(subj, "1 new task") || !strings.Contains(body, "#2") || !strings.Contains(body, "Erica Rapa") {
		t.Errorf("mail = %q / %q", subj, body)
	}
}

func TestUnattended_TwoSessionsInAWindowIsNoConsole(t *testing.T) {
	two := []Session{sess("collab", "busy", 0), func() Session { s := sess("collab", "idle", time.Hour); s.Pane = "%9"; return s }()}
	seen := Seen{Key(UnattendedWindow, "collaboratory"): {}}
	got, _ := Unattended(cfg(), two, []string{"collaboratory"}, []Task{{7, "collaboratory", "", t0, "x", false}}, seen, t0.Add(time.Hour))
	if len(got) != 1 {
		t.Errorf("a window Plan will not nudge (two sessions) counted as watched: %+v", got)
	}
}

// swb 758: switchboard was mapped to window "sbc", the session ran in window
// "swb", and every switchboard task emailed "no console". A session whose repo
// declares its project watches it under any window name.
func TestDeclaredProjects_WatchUnderAnyWindowName(t *testing.T) {
	swb := sess("swb", "idle", time.Hour) // no push.json mapping for "swb"
	if len(cfg().QueuesOf(swb)) != 0 {
		t.Fatalf("POSITIVE CONTROL: window swb is unmapped in cfg(); QueuesOf = %v", cfg().QueuesOf(swb))
	}
	seen := Seen{Key(UnattendedWindow, "switchboard"): {}}
	tasks := []Task{{9, "switchboard", "", t0.Add(-time.Minute), "x", false}}
	if got, _ := Unattended(cfg(), []Session{swb}, []string{"switchboard"}, tasks, seen, t0.Add(time.Hour)); len(got) != 1 {
		t.Fatalf("without a declaration the renamed window should be unwatched: %+v", got)
	}

	swb.Projects = []string{"switchboard"}
	seen = Seen{Key(UnattendedWindow, "switchboard"): {}}
	if got, _ := Unattended(cfg(), []Session{swb}, []string{"switchboard"}, tasks, seen, t0.Add(time.Hour)); len(got) != 0 {
		t.Errorf("a declared session did not count as watching: emailed %+v", got)
	}
	pseen := Seen{Key("swb", "switchboard"): {}}
	n := Plan(cfg(), []Session{swb}, tasks, pseen, t0, always)
	if len(n) != 1 || n[0].Pane != "%swb" || !strings.Contains(n[0].Text, "switchboard queue") {
		t.Errorf("a declared session was not nudged: %+v", n)
	}
}

// The declaration wins over the window's mapping: a repo that says "foundry"
// in a window push.json maps to collab watches foundry only.
func TestDeclaredProjects_OverrideTheWindowMapping(t *testing.T) {
	s := sess("collab", "idle", time.Hour)
	s.Projects = []string{"foundry"}
	if got := cfg().QueuesOf(s); len(got) != 1 || got[0] != "foundry" {
		t.Errorf("QueuesOf = %v, want [foundry]", got)
	}
	if got := cfg().QueuesOf(sess("collab", "idle", time.Hour)); len(got) != 2 {
		t.Errorf("an undeclared session lost its window mapping: %v", got)
	}
}

// swb 758 review: a task the no-console memory holds unsettled (the batched
// mail had not gone out yet) must not be swallowed when a session starts
// watching under a (window, slug) key it has never had. Unattended settles a
// watched slug silently, so a silent first sight here would lose it entirely.
func TestPlan_FirstSightKeepsWhatTheMailStillOwes(t *testing.T) {
	now := t0.Add(time.Hour)
	owed := Task{7, "switchboard", "", t0, "arrived while unwatched", false}
	known := Task{8, "switchboard", "", t0.Add(-time.Hour), "already settled", false}
	seen := Seen{Key(UnattendedWindow, "switchboard"): {8: known.At}} // 7 returned, mail throttled
	swb := sess("swb", "idle", time.Hour)
	swb.Projects = []string{"switchboard"}
	n := Plan(cfg(), []Session{swb}, []Task{owed, known}, seen, now, always)
	if len(n) != 1 || len(n[0].Settle[Key("swb", "switchboard")]) != 1 {
		t.Fatalf("first sight: nudges %+v, want one for #7 only", n)
	}
	if _, ok := n[0].Settle[Key("swb", "switchboard")][7]; !ok {
		t.Errorf("the owed task is not what the nudge settles: %+v", n[0].Settle)
	}
	if _, ok := seen[Key("swb", "switchboard")][8]; !ok {
		t.Errorf("a task the mail memory already settled should be recorded silently")
	}
	// A slug the no-console memory has never seen keeps the old rule: silent.
	fresh := Seen{}
	if n := Plan(cfg(), []Session{swb}, []Task{owed}, fresh, now, always); len(n) != 0 {
		t.Errorf("first sight with no mail memory at all nudged: %+v", n)
	}
}

// swb 758, 11:53:35: the session created #758, a pass ran before its
// task_signal a second later, and the session was nudged about its own task.
func TestGrace_AFreshTaskWaitsOnePass(t *testing.T) {
	seen := primed()
	fresh := []Task{{9, "switchboard", "", t0.Add(-time.Second), "", false}}
	if n := Plan(cfg(), []Session{sess("sbc", "busy", 0)}, fresh, seen, t0, always); len(n) != 0 {
		t.Fatalf("a task one second old nudged: %+v", n)
	}
	if _, ok := seen[Key("sbc", "switchboard")][9]; ok {
		t.Fatalf("a fresh task was recorded, so it could never nudge later")
	}
	// Next pass the creating session has signalled it: its own, never news.
	mine := []Task{{9, "switchboard", "sbc", t0.Add(-time.Second), "", false}}
	if n := Plan(cfg(), []Session{sess("sbc", "busy", 0)}, mine, seen, t0.Add(30*time.Second), always); len(n) != 0 {
		t.Errorf("own task nudged after the grace: %+v", n)
	}
	// Nobody claims it: after the grace it is news like any other.
	other := primed()
	Plan(cfg(), []Session{sess("sbc", "busy", 0)}, fresh, other, t0, always)
	if n := Plan(cfg(), []Session{sess("sbc", "busy", 0)}, fresh, other, t0.Add(30*time.Second), always); len(n) != 1 {
		t.Errorf("an unclaimed task never nudged after the grace: %+v", n)
	}
	nc := Seen{Key(UnattendedWindow, "town-ai"): {}}
	tt := []Task{{10, "town-ai", "", t0.Add(-time.Second), "x", false}}
	if got, _ := Unattended(cfg(), nil, []string{"town-ai"}, tt, nc, t0); len(got) != 0 {
		t.Errorf("a task one second old mailed: %+v", got)
	}
	if got, _ := Unattended(cfg(), nil, []string{"town-ai"}, tt, nc, t0.Add(30*time.Second)); len(got) != 1 {
		t.Errorf("after the grace it did not mail: %+v", got)
	}
}

// swb 758 re-review: a fresh task in a WATCHED project must be recorded
// nowhere, or a console that is gone by the next pass loses it: Plan deferred
// it, and Unattended would already hold it as settled.
func TestGrace_FreshTaskOfAConsoleThatVanishesStillMails(t *testing.T) {
	seen := Seen{Key(UnattendedWindow, "switchboard"): {}}
	primed := primed()
	for k, v := range primed {
		seen[k] = v
	}
	sbc := []Session{sess("sbc", "busy", 0)}
	tt := []Task{{11, "switchboard", "", t0.Add(-time.Second), "x", false}}
	if n := Plan(cfg(), sbc, tt, seen, t0, always); len(n) != 0 {
		t.Fatalf("fresh task nudged: %+v", n)
	}
	if got, _ := Unattended(cfg(), sbc, []string{"switchboard"}, tt, seen, t0); len(got) != 0 {
		t.Fatalf("fresh task mailed: %+v", got)
	}
	if _, ok := seen[Key(UnattendedWindow, "switchboard")][11]; ok {
		t.Fatalf("a fresh task of a watched project was settled in the no-console memory")
	}
	// Next pass the console is gone: it must mail.
	if got, _ := Unattended(cfg(), nil, []string{"switchboard"}, tt, seen, t0.Add(30*time.Second)); len(got) != 1 {
		t.Errorf("the task was lost: neither nudged nor mailed")
	}
}

// swb 767: #764 was created by the live keto-track session (project personal,
// which no console watches) and emailed "no console" 44 s later. A task a
// console created is that console's; only outside arrivals email.
func TestUnattended_ConsoleCreatedTasksNeverMail(t *testing.T) {
	seen := Seen{Key(UnattendedWindow, "personal"): {}}
	mine := Task{ID: 764, Slug: "personal", At: t0, Title: "keto-track: auto clean check-in", ByConsole: true}
	outside := Task{ID: 770, Slug: "personal", At: t0, Title: "Fwd: invoice"}
	got, _ := Unattended(cfg(), nil, []string{"personal"}, []Task{mine, outside}, seen, t0.Add(time.Hour))
	if len(got) != 1 || got[0].ID != 770 {
		t.Fatalf("got %+v, want only the outside arrival #770", got)
	}
	// A console-created task still nudges the console that watches its project
	// (a handoff like "kube: roll keto-track" must reach kube).
	handoff := Task{ID: 771, Slug: "switchboard", At: t0.Add(-time.Minute), ByConsole: true}
	if n := Plan(cfg(), []Session{sess("sbc", "busy", 0)}, []Task{handoff}, primed(), t0, always); len(n) != 1 {
		t.Errorf("a console-created task in a watched project did not nudge: %+v", n)
	}
}
