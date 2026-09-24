// Package swbpush decides when to nudge a local Claude Code session that its
// swb queue has something new (swb-push, 2026-09-24).
//
// Salvador: "just new stuff on the q, claude would figure out" and "if the
// session is idle can start working if not queue behind current work". So a
// nudge carries no details, only which queue changed; the session reads the
// queue and starts. An idle session gets it typed into its input box; a busy
// one gets it from ~/.claude/swb-hook.py when its current turn ends.
//
// Everything here is pure, a function of values the command reads (the hook's
// session files, tmux captures, one read-only SELECT). cmd/swb-push does the I/O.
package swbpush

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Config is ~/.claude/swb/push.json.
type Config struct {
	PollS    int                 `json:"poll_s"`
	MinIdleS int                 `json:"min_idle_s"`
	Windows  map[string][]string `json:"windows"` // tmux window name -> project slugs
	// MailEveryS batches the "no console" email (swb #610): at most one per
	// this many seconds, listing every unattended task since the last one.
	MailEveryS int `json:"mail_every_s"`
	// NotifySkip lists projects whose unattended tasks never email.
	NotifySkip []string `json:"notify_skip"`
}

// Defaults fills unset fields.
func (c Config) Defaults() Config {
	if c.PollS <= 0 {
		c.PollS = 30
	}
	if c.MinIdleS <= 0 {
		c.MinIdleS = 20
	}
	if c.MailEveryS <= 0 {
		c.MailEveryS = 600
	}
	return c
}

// Session is one live Claude session: what ~/.claude/swb-hook.py recorded,
// joined to what tmux says about its pane right now.
type Session struct {
	SID        string
	Pane       string    // hook: $TMUX_PANE
	Window     string    // hook: the session name it computed (tmux window name)
	Turn       string    // hook: busy | idle | prompt | gone
	TurnAt     time.Time // hook: when Turn last changed
	LiveWindow string    // tmux, now
	LiveCmd    string    // tmux pane_current_command, now
}

// Task is one in-play task of a mapped project. At is the newest of
// created_at, activity_at and surfaced_at; Session is working_session while
// working_state is set, else "".
type Task struct {
	ID      int64
	Slug    string
	Session string
	At      time.Time
	Title   string // for the "no console" email only; a nudge never carries it
}

// Seen is the watcher's memory, per (window, project): the At it last settled
// for each task. Per task rather than one high-water mark, so a transaction
// that commits late (its now() older than a task already seen) still counts.
type Seen map[string]map[int64]time.Time

// Nudge is one line for a session, and what it settles once delivered.
//
// An IDLE session gets it typed into its input box (Typed). A BUSY session is
// never typed into: a keystroke could land on a permission dialog or a menu
// opening mid-turn. Its nudge goes to a file that ~/.claude/swb-hook.py hands
// to Claude when the turn ends (a Stop-hook continuation), which is exactly
// "queue behind current work".
type Nudge struct {
	SID, Pane, Window, Text string
	Typed                   bool
	Settle                  Seen
}

// Key is Seen's key: one per (window, project).
func Key(window, slug string) string { return window + "|" + slug }

// Text is the nudge. No details by design.
func Text(slugs []string) string {
	return fmt.Sprintf("swb: new in the %s queue — check it and start on what's yours", strings.Join(slugs, ", "))
}

// Apply settles s into seen.
func (seen Seen) Apply(s Seen) {
	for k, m := range s {
		if seen[k] == nil {
			seen[k] = map[int64]time.Time{}
		}
		for id, at := range m {
			seen[k][id] = at
		}
	}
}

// Plan decides this pass's nudges and updates seen for everything that needs
// no nudge: a queue seen for the first time (only what arrives after counts),
// the session's own tasks (so releasing one later is not news), and tasks that
// left play (pruned). What a nudge would settle is returned in Nudge.Settle;
// the caller applies it only after the nudge was submitted, so a missed chance
// is retried next pass.
//
// A session is nudged only when all of these hold:
//   - its window is mapped, the hook's name for it is still the pane's window
//     name, and it is the window's ONLY live session (two Claude panes in one
//     window would both be told to start on the same work);
//   - the pane runs claude;
//   - it is busy (the nudge goes to the hook, never typed), or idle for at
//     least MinIdleS (a moment for him to start typing after a turn ends) with
//     composerEmpty(pane): the input box on screen and empty; never on a
//     permission prompt.
func Plan(cfg Config, sessions []Session, tasks []Task, seen Seen, now time.Time,
	composerEmpty func(pane string) bool) []Nudge {
	cfg = cfg.Defaults()
	perWindow := map[string]int{}
	for _, s := range sessions {
		if s.LiveCmd == "claude" { // a dead session's leftover file must not freeze the window
			perWindow[s.LiveWindow]++
		}
	}
	var out []Nudge
	for _, s := range sessions {
		slugs, ok := cfg.Windows[s.LiveWindow]
		if !ok || s.Window != s.LiveWindow || perWindow[s.LiveWindow] != 1 {
			continue
		}
		var changed []string
		settle := Seen{}
		for _, slug := range slugs {
			key := Key(s.LiveWindow, slug)
			first := seen[key] == nil
			if first {
				seen[key] = map[int64]time.Time{}
			}
			live := map[int64]bool{}
			for _, t := range tasks {
				if t.Slug != slug {
					continue
				}
				live[t.ID] = true
				prev, known := seen[key][t.ID]
				switch {
				case first || t.Session == s.LiveWindow:
					seen[key][t.ID] = t.At
				case !known || t.At.After(prev):
					if settle[key] == nil {
						settle[key] = map[int64]time.Time{}
					}
					settle[key][t.ID] = t.At
				}
			}
			for id := range seen[key] {
				if !live[id] {
					delete(seen[key], id)
				}
			}
			if len(settle[key]) > 0 {
				changed = append(changed, slug)
			}
		}
		if len(changed) == 0 || s.LiveCmd != "claude" {
			continue
		}
		typed := false
		switch s.Turn {
		case "busy":
		case "idle":
			if now.Sub(s.TurnAt) < time.Duration(cfg.MinIdleS)*time.Second || !composerEmpty(s.Pane) {
				continue
			}
			typed = true
		default: // prompt, gone, unknown
			continue
		}
		sort.Strings(changed)
		out = append(out, Nudge{SID: s.SID, Pane: s.Pane, Window: s.LiveWindow, Text: Text(changed), Typed: typed, Settle: settle})
	}
	return out
}

var (
	ansi    = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	dimSpan = regexp.MustCompile(`\x1b\[2m.*?(\x1b\[0m|\x1b\[22m|$)`)
	// prompt is Claude's input-box marker: U+276F then a NO-BREAK SPACE, built
	// from their values (an NBSP pasted into source is invisible).
	prompt = string(rune(0x276F)) + string(rune(0x00A0))
)

func isRule(line string) bool {
	p := strings.TrimSpace(ansi.ReplaceAllString(line, ""))
	return p != "" && strings.Trim(p, "─") == ""
}

// Composer reads `tmux capture-pane -e -p` output and returns the text in
// Claude's input box, and whether the box is on screen. The box is the lines
// between two rules of "─", the first starting with the prompt marker. Dim
// spans are Claude's suggestion placeholder, not his typing, and are dropped.
// No box (a permission dialog, a menu, not Claude) is ok=false.
func Composer(capture string) (text string, ok bool) {
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	for b := len(lines) - 1; b > 1; b-- {
		if !isRule(lines[b]) {
			continue
		}
		for a := b - 2; a >= 0; a-- {
			if !isRule(lines[a]) {
				continue
			}
			if !strings.Contains(lines[a+1], prompt) {
				break
			}
			var parts []string
			for i := a + 1; i < b; i++ {
				l := lines[i]
				if i == a+1 {
					l = strings.SplitN(l, prompt, 2)[1]
				}
				parts = append(parts, ansi.ReplaceAllString(dimSpan.ReplaceAllString(l, ""), ""))
			}
			return strings.TrimSpace(strings.Join(parts, "\n")), true
		}
		return "", false // the bottom-most rule has no box above it
	}
	return "", false
}

// ComposerEmpty: the input box is on screen with nothing typed in it.
func ComposerEmpty(capture string) bool {
	text, ok := Composer(capture)
	return ok && text == ""
}

// ComposerHolds: the input box is on screen and holds exactly text (Claude
// wraps long input, so whitespace is not compared). The watcher presses Enter
// only when this is true, so an Enter never lands on a dialog that opened, or
// a draft he started, between typing the nudge and submitting it.
func ComposerHolds(capture, text string) bool {
	got, ok := Composer(capture)
	return ok && squash(got) == squash(text)
}

func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// UnattendedWindow is Seen's pseudo-window for tasks no console watches.
const UnattendedWindow = "_no_console"

// Unattended is swb #610's second half (Salvador: "the watcher should notify me
// a task landed and there is no console for it"): the tasks that are new since
// last settled, in projects with NO live Claude session in a window mapped to
// them, minus NotifySkip. Same memory rules as Plan: a project seen for the
// first time counts only what arrives after, out-of-play tasks are pruned, and
// the caller applies the returned Seen only after the email went out.
//
// projects is every project slug: a project with no task in play yet is still
// "seen", so its FIRST task emails (town-ai's case) instead of passing as a
// first sight.
func Unattended(cfg Config, sessions []Session, projects []string, tasks []Task, seen Seen) ([]Task, Seen) {
	cfg = cfg.Defaults()
	// Watched means Plan can nudge it: the window's ONLY live Claude session
	// (Plan skips a window with two), under its own name.
	perWindow := map[string]int{}
	for _, s := range sessions {
		if s.LiveCmd == "claude" {
			perWindow[s.LiveWindow]++
		}
	}
	watched := map[string]bool{}
	for _, s := range sessions {
		if s.LiveCmd != "claude" || s.Window != s.LiveWindow || perWindow[s.LiveWindow] != 1 {
			continue
		}
		for _, slug := range cfg.Windows[s.LiveWindow] {
			watched[slug] = true
		}
	}
	skip := map[string]bool{}
	for _, slug := range cfg.NotifySkip {
		skip[slug] = true
	}
	bySlug := map[string][]Task{}
	for _, slug := range projects {
		bySlug[slug] = nil
	}
	for _, t := range tasks {
		bySlug[t.Slug] = append(bySlug[t.Slug], t)
	}
	var out []Task
	settle := Seen{}
	for slug, ts := range bySlug {
		key := Key(UnattendedWindow, slug)
		first := seen[key] == nil
		if first {
			seen[key] = map[int64]time.Time{}
		}
		live := map[int64]bool{}
		for _, t := range ts {
			live[t.ID] = true
			prev, known := seen[key][t.ID]
			switch {
			case first || watched[slug] || skip[slug]:
				// a console has it (its nudge is the notice), or it is muted
				seen[key][t.ID] = t.At
			case !known || t.At.After(prev):
				if settle[key] == nil {
					settle[key] = map[int64]time.Time{}
				}
				settle[key][t.ID] = t.At
				out = append(out, t)
			}
		}
		for id := range seen[key] {
			if !live[id] {
				delete(seen[key], id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].ID < out[j].ID
	})
	return out, settle
}

// Mail is the "no console" email: subject and body.
func Mail(tasks []Task) (subject, body string) {
	subject = fmt.Sprintf("swb: %d new task(s) with no console", len(tasks))
	var b strings.Builder
	b.WriteString("New in swb, and no Claude session is watching these projects:\n\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "  #%d  %-14s %s\n", t.ID, t.Slug, t.Title)
	}
	b.WriteString("\nMap a window to the project in ~/.claude/swb/push.json, or open a session for it.\n")
	return subject, b.String()
}
