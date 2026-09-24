// Command swb-push types a one-line nudge into a local Claude Code session when
// its swb queue has something new. Runs on the workstation as a systemd user
// service (~/.config/systemd/user/swb-push.service); see internal/swbpush for
// the rules.
//
// Inputs, all read-only: the session files ~/.claude/swb-hook.py writes
// (~/.claude/swb/sessions/*.json: pane, window, turn, turn_at), tmux (live
// panes, screen captures), and one SELECT per pass in a READ ONLY transaction.
// It calls no tool and writes nothing to the ops database.
//
// A BUSY session is never typed into. Its nudge is written to
// ~/.claude/swb/nudges/<session_id>.txt, and swb-hook.py's Stop hook hands it
// to Claude as a continuation when the turn ends ("queue behind current work").
//
// An IDLE session gets the line typed, in two steps, and the second is
// verified: type, re-read the screen, press Enter only if the input box holds
// exactly that line. If anything changed in between (he started typing, a
// dialog), nothing is submitted; the line waits as "pending" and is submitted
// on a later pass once the box shows it alone again.
//
// A task that lands where NO console watches (no live Claude session in a
// window mapped to its project) emails him instead, batched: at most one mail
// per mail_every_s (default 10 min), through ~/.claude/notify-email.py
// (swb #610: "the watcher should notify me a task landed and there is no
// console for it").
//
// One instance at a time: an flock on ~/.claude/swb/push.lock.
//
//	swb-push          run until SIGINT/SIGTERM
//	swb-push once     one pass, then exit
//	swb-push status   sessions, their queues, what is new for each
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/swbpush"
	"github.com/sspataro57/switchboard/internal/tools"
)

var (
	root     = filepath.Join(os.Getenv("HOME"), ".claude", "swb")
	sessDir  = filepath.Join(root, "sessions")
	nudgeDir = filepath.Join(root, "nudges")
	confPath = filepath.Join(root, "push.json")
	seenPath = filepath.Join(root, "push-state.json")
)

const tmuxTimeout = 5 * time.Second

// pending is a nudge typed but not yet submitted.
type pending struct {
	text   string
	settle swbpush.Seen
}

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode != "" && mode != "once" && mode != "status" {
		fmt.Fprintln(os.Stderr, "usage: swb-push [once|status]")
		os.Exit(2)
	}
	if mode != "status" {
		if f, err := os.OpenFile(filepath.Join(root, "push.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			log.SetOutput(f)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := store.NewPoolDSN(ctx, databaseURL())
	if err != nil {
		log.Fatalf("swb-push: %v", err)
	}
	defer pool.Close()

	if mode == "status" {
		status(ctx, pool)
		return
	}
	if vimMode() {
		log.Printf("refusing to run: Claude Code editorMode is vim, where a typed line runs as editor commands")
		os.Exit(3) // the unit's RestartPreventExitStatus: no restart loop
	}
	lock, err := os.OpenFile(filepath.Join(root, "push.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		log.Fatalf("swb-push: open lock: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "swb-push: another instance is running (the service?); not starting")
		os.Exit(3)
	}
	log.Printf("start (%s), windows=%v", map[bool]string{true: "once", false: "loop"}[mode == "once"], loadConfig().Windows)
	waiting := map[string]pending{}
	var lastMail time.Time
	for {
		cfg := loadConfig() // edits to push.json apply on the next pass
		if err := pass(ctx, pool, cfg, waiting, &lastMail); err != nil {
			log.Printf("pass failed: %v", err)
		}
		if mode == "once" {
			return
		}
		select {
		case <-ctx.Done():
			for pane := range waiting {
				log.Printf("stop: a typed nudge in %s was never submitted and stays in its input box", pane)
			}
			log.Printf("stop")
			return
		case <-time.After(time.Duration(cfg.PollS) * time.Second):
		}
	}
}

func pass(ctx context.Context, pool *pgxpool.Pool, cfg swbpush.Config, waiting map[string]pending, lastMail *time.Time) error {
	projects, tasks, err := queueTasks(ctx, pool)
	if err != nil {
		return err
	}
	seen, err := loadSeen()
	if err != nil {
		return err // never guess: a lost memory would re-nudge or miss
	}
	sessions := liveSessions()
	finishPending(sessions, seen, waiting)
	notWaiting := func(pane string) bool {
		_, busy := waiting[pane]
		return !busy && composerEmpty(pane)
	}
	for _, n := range swbpush.Plan(cfg, sessions, tasks, seen, time.Now(), notWaiting) {
		if !n.Typed {
			if err := leaveForHook(n.SID, n.Text); err != nil {
				log.Printf("nudge %s (%s): %v", n.Window, n.Pane, err)
				continue
			}
			seen.Apply(n.Settle)
			log.Printf("queued for %s (%s, busy; the hook delivers at turn end): %s", n.Window, n.Pane, n.Text)
			continue
		}
		if err := tmux("send-keys", "-t", n.Pane, "-l", n.Text); err != nil {
			log.Printf("nudge %s (%s): type failed: %v", n.Window, n.Pane, err)
			continue
		}
		time.Sleep(150 * time.Millisecond)
		if submitIfHeld(n.Pane, n.Text) {
			seen.Apply(n.Settle)
			log.Printf("nudged %s (%s): %s", n.Window, n.Pane, n.Text)
			continue
		}
		waiting[n.Pane] = pending{n.Text, n.Settle}
		log.Printf("nudge %s (%s) typed but NOT submitted: the input box changed before Enter; pending", n.Window, n.Pane)
	}
	unwatched, settle := swbpush.Unattended(cfg, sessions, projects, tasks, seen)
	if len(unwatched) > 0 && time.Since(*lastMail) >= time.Duration(cfg.MailEveryS)*time.Second {
		subject, body := swbpush.Mail(unwatched)
		if err := mailHim(subject, body); err != nil {
			log.Printf("no-console mail for %d task(s) failed (retried next pass): %v", len(unwatched), err)
		} else {
			seen.Apply(settle)
			*lastMail = time.Now()
			log.Printf("mailed: %s", subject)
		}
	}
	return saveSeen(seen)
}

// mailHim sends through the workstation's notifier (the masked alias it is
// configured for). "send now" mode: the notifier's idle on/off switch is for
// idle pings, not for this.
func mailHim(subject, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("HOME"), ".claude", "notify-email.py"), body, subject, "0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("notify-email.py: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// finishPending submits a pending nudge once the pane's box holds it alone
// again, and forgets it when he replaced or cleared it.
func finishPending(sessions []swbpush.Session, seen swbpush.Seen, waiting map[string]pending) {
	turn := map[string]string{}
	for _, s := range sessions {
		turn[s.Pane] = s.Turn
	}
	for pane, p := range waiting {
		t, live := turn[pane]
		if !live {
			delete(waiting, pane)
			continue
		}
		capture, err := capturePane(pane)
		if err != nil {
			continue
		}
		text, box := swbpush.Composer(capture)
		switch {
		case !box || t != "idle":
			// a dialog is up, or the session is busy (Enter only ever goes to an
			// idle session): wait
		case swbpush.ComposerHolds(capture, p.text):
			if submitIfHeld(pane, p.text) {
				seen.Apply(p.settle)
				delete(waiting, pane)
				log.Printf("pending nudge submitted in %s", pane)
			}
		case text == "":
			seen.Apply(p.settle) // he submitted or cleared it: either way, seen
			delete(waiting, pane)
		default:
			delete(waiting, pane)
			log.Printf("pending nudge in %s dropped: the box holds other text now", pane)
		}
	}
}

// leaveForHook writes a busy session's nudge where swb-hook.py's Stop hook
// finds it. A newer nudge replaces an undelivered one: the text names every
// queue that changed since the session last heard.
func leaveForHook(sid, text string) error {
	if err := os.MkdirAll(nudgeDir, 0o755); err != nil {
		return fmt.Errorf("nudge dir: %w", err)
	}
	p := filepath.Join(nudgeDir, sid+".txt")
	if old, err := os.ReadFile(p); err == nil && len(old) > 0 && string(old) != text {
		text = mergeText(string(old), text)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return fmt.Errorf("write nudge: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("place nudge: %w", err)
	}
	return nil
}

var queuesRE = regexp.MustCompile(`new in the (.*) queue`)

// mergeText joins the queues two nudge lines name.
func mergeText(a, b string) string {
	set := map[string]bool{}
	for _, t := range []string{a, b} {
		if m := queuesRE.FindStringSubmatch(t); m != nil {
			for _, q := range strings.Split(m[1], ", ") {
				set[q] = true
			}
		}
	}
	var qs []string
	for q := range set {
		qs = append(qs, q)
	}
	sort.Strings(qs)
	return swbpush.Text(qs)
}

// submitIfHeld presses Enter only when the box holds exactly text.
func submitIfHeld(pane, text string) bool {
	capture, err := capturePane(pane)
	if err != nil || !swbpush.ComposerHolds(capture, text) {
		return false
	}
	return tmux("send-keys", "-t", pane, "Enter") == nil
}

// queueTasks reads every project slug and every in-play task, in one READ
// ONLY transaction.
func queueTasks(ctx context.Context, pool *pgxpool.Pool) ([]string, []swbpush.Task, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("begin read-only: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var projects []string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(slug ORDER BY slug), '{}') FROM projects`).Scan(&projects); err != nil {
		return nil, nil, fmt.Errorf("read projects: %w", err)
	}
	rs, err := tx.Query(ctx,
		`SELECT t.id, p.slug,
		        CASE WHEN t.working_state IS NOT NULL THEN COALESCE(t.working_session, '') ELSE '' END,
		        GREATEST(t.created_at, COALESCE(t.activity_at, t.created_at), COALESCE(t.surfaced_at, t.created_at)),
		        t.title
		   FROM tasks t JOIN projects p ON p.id = t.project_id
		  WHERE `+tools.InPlayPredicate)
	if err != nil {
		return nil, nil, fmt.Errorf("read queues: %w", err)
	}
	defer rs.Close()
	var out []swbpush.Task
	for rs.Next() {
		var t swbpush.Task
		if err := rs.Scan(&t.ID, &t.Slug, &t.Session, &t.At, &t.Title); err != nil {
			return nil, nil, fmt.Errorf("scan queue row: %w", err)
		}
		out = append(out, t)
	}
	if err := rs.Err(); err != nil {
		return nil, nil, fmt.Errorf("read queues: %w", err)
	}
	return projects, out, nil
}

// hookState is the part of a swb-hook.py session file swb-push reads.
type hookState struct {
	Pane   *string  `json:"pane"`
	Window string   `json:"window"`
	Turn   string   `json:"turn"`
	TurnAt *float64 `json:"turn_at"`
}

// liveSessions is the newest session file for each live tmux pane.
func liveSessions() []swbpush.Session {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{pane_id}\t#{window_name}\t#{pane_current_command}").Output()
	if err != nil {
		return nil
	}
	type pane struct{ win, cmd string }
	panes := map[string]pane{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			panes[f[0]] = pane{f[1], f[2]}
		}
	}
	best := map[string]swbpush.Session{}
	files, _ := filepath.Glob(filepath.Join(sessDir, "*.json"))
	for _, fn := range files {
		b, err := os.ReadFile(fn)
		if err != nil {
			continue
		}
		var st hookState
		if json.Unmarshal(b, &st) != nil || st.Pane == nil || st.TurnAt == nil || st.Turn == "" || st.Turn == "gone" {
			continue
		}
		p, ok := panes[*st.Pane]
		if !ok {
			continue
		}
		at := time.Unix(0, int64(*st.TurnAt*1e9))
		if cur, ok := best[*st.Pane]; ok && !at.After(cur.TurnAt) {
			continue
		}
		best[*st.Pane] = swbpush.Session{SID: strings.TrimSuffix(filepath.Base(fn), ".json"), Pane: *st.Pane,
			Window: st.Window, Turn: st.Turn, TurnAt: at, LiveWindow: p.win, LiveCmd: p.cmd}
	}
	list := make([]swbpush.Session, 0, len(best))
	for _, s := range best {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LiveWindow < list[j].LiveWindow })
	return list
}

func capturePane(pane string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "capture-pane", "-e", "-p", "-t", pane).Output()
	if err != nil {
		return "", fmt.Errorf("capture %s: %w", pane, err)
	}
	return string(out), nil
}

func composerEmpty(pane string) bool {
	capture, err := capturePane(pane)
	return err == nil && swbpush.ComposerEmpty(capture)
}

// tmux runs one tmux command with a timeout. Deliberately not tied to the
// service's signal context: a stop must not land between typing a line and
// the check that decides whether to submit it.
func tmux(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux %s: %w (%s)", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func loadConfig() swbpush.Config {
	var c swbpush.Config
	if b, err := os.ReadFile(confPath); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			log.Printf("push.json: %v (no windows mapped this pass)", err)
		}
	}
	return c.Defaults()
}

func loadSeen() (swbpush.Seen, error) {
	s := swbpush.Seen{}
	b, err := os.ReadFile(seenPath)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", seenPath, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s (fix or delete it; deleting starts fresh): %w", seenPath, err)
	}
	return s, nil
}

func saveSeen(s swbpush.Seen) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return fmt.Errorf("encode seen: %w", err)
	}
	tmp := seenPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, seenPath); err != nil {
		return fmt.Errorf("replace %s: %w", seenPath, err)
	}
	return nil
}

// vimMode: Claude Code's vim editor mode would run a typed line as NORMAL-mode
// commands. Checked in both places the setting can live.
func vimMode() bool {
	home := os.Getenv("HOME")
	for _, p := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json")} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v struct {
			EditorMode string `json:"editorMode"`
		}
		if json.Unmarshal(b, &v) == nil && v.EditorMode == "vim" {
			return true
		}
	}
	return false
}

func status(ctx context.Context, pool *pgxpool.Pool) {
	cfg := loadConfig()
	_, tasks, err := queueTasks(ctx, pool)
	if err != nil {
		fmt.Println("queues:", err)
	}
	seen, err := loadSeen()
	if err != nil {
		fmt.Println("seen:", err)
		seen = swbpush.Seen{}
	}
	for _, s := range liveSessions() {
		fmt.Printf("%-10s %-5s turn=%-6s for %-8s queues=%v\n", s.LiveWindow, s.Pane, s.Turn,
			time.Since(s.TurnAt).Round(time.Second), cfg.Windows[s.LiveWindow])
		for _, slug := range cfg.Windows[s.LiveWindow] {
			m := seen[swbpush.Key(s.LiveWindow, slug)]
			if m == nil {
				fmt.Printf("    %-14s not looked at yet\n", slug)
				continue
			}
			var fresh []int64
			for _, t := range tasks {
				if at, ok := m[t.ID]; t.Slug == slug && t.Session != s.LiveWindow && (!ok || t.At.After(at)) {
					fresh = append(fresh, t.ID)
				}
			}
			fmt.Printf("    %-14s %d tracked, new: %v\n", slug, len(m), fresh)
		}
	}
}

var exportRE = regexp.MustCompile(`^\s*export\s+OPS_DATABASE_URL=(.+)$`)

// databaseURL: the environment, else the export in ~/.bashrc (a systemd user
// service starts without the interactive shell's exports).
func databaseURL() string {
	for _, k := range []string{"OPS_DATABASE_URL", "DATABASE_URL"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	b, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".bashrc"))
	for _, line := range strings.Split(string(b), "\n") {
		if m := exportRE.FindStringSubmatch(line); m != nil {
			return strings.Trim(strings.TrimSpace(m[1]), `'"`)
		}
	}
	return ""
}
