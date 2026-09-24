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
	tasks := []Task{{1, "collaboratory", "", t0.Add(-time.Hour)}}
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
		seen := primed(Task{1, "collaboratory", "", t0.Add(-time.Hour)})
		tasks := []Task{{1, "collaboratory", "", t0.Add(-time.Hour)}, {2, "collaboratory", "", t0.Add(-time.Minute)}}
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
	seen := primed(Task{2, "collaboratory", "", t0})
	tasks := []Task{{2, "collaboratory", "", t0}, {3, "collaboratory", "", t0.Add(-time.Second)}}
	if n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, tasks, seen, t0, always); len(n) != 1 {
		t.Fatalf("late-committed task not noticed: %+v", n)
	}
}

func TestPlan_ActivityOnAKnownTaskCounts(t *testing.T) {
	seen := primed(Task{1, "collaboratory", "", t0.Add(-time.Hour)})
	tasks := []Task{{1, "collaboratory", "", t0}}
	if n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, tasks, seen, t0, always); len(n) != 1 {
		t.Fatalf("new activity on a known task not noticed: %+v", n)
	}
}

func TestPlan_Gates(t *testing.T) {
	tasks := []Task{{2, "collaboratory", "", t0}}
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
	n := Plan(cfg(), []Session{sess("collab", "busy", 0)}, []Task{{2, "collaboratory", "", t0}}, primed(), t0,
		func(string) bool { t.Fatal("a busy session's screen was read"); return false })
	if len(n) != 1 || n[0].Typed {
		t.Fatalf("busy: %+v, want one hook nudge", n)
	}
}

func TestPlan_DeadPaneDoesNotFreezeTheWindow(t *testing.T) {
	dead := sess("collab", "idle", time.Hour)
	dead.Pane, dead.LiveCmd = "%9", "bash"
	n := Plan(cfg(), []Session{sess("collab", "busy", 0), dead}, []Task{{2, "collaboratory", "", t0}}, primed(), t0, always)
	if len(n) != 1 || n[0].Pane != "%collab" {
		t.Fatalf("a leftover non-claude pane blocked the window: %+v", n)
	}
}

func TestPlan_OwnTasksNeverNudge_EvenWhenReleased(t *testing.T) {
	seen := primed()
	mine := []Task{{5, "switchboard", "sbc", t0.Add(-time.Minute)}}
	if n := Plan(cfg(), []Session{sess("sbc", "idle", time.Hour)}, mine, seen, t0, always); len(n) != 0 {
		t.Fatalf("nudged about its own task: %+v", n)
	}
	released := []Task{{5, "switchboard", "", t0.Add(-time.Minute)}}
	if n := Plan(cfg(), []Session{sess("sbc", "idle", time.Hour)}, released, seen, t0, always); len(n) != 0 {
		t.Errorf("releasing its own task nudged the session: %+v", n)
	}
	other := append(released, Task{6, "switchboard", "collab", t0})
	if n := Plan(cfg(), []Session{sess("sbc", "idle", time.Hour)}, other, seen, t0, always); len(n) != 1 {
		t.Errorf("another session's task is news for this one: %+v", n)
	}
}

func TestPlan_PrunesTasksOutOfPlay(t *testing.T) {
	seen := primed(Task{1, "collaboratory", "", t0})
	Plan(cfg(), []Session{sess("collab", "busy", 0)}, nil, seen, t0, always)
	if _, ok := seen[Key("collab", "collaboratory")][1]; ok {
		t.Errorf("a task out of play stays in memory")
	}
}

func TestPlan_TwoQueuesOneLine(t *testing.T) {
	tasks := []Task{{1, "collaboratory", "", t0}, {2, "a-millon", "", t0}}
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
