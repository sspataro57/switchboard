package main

// SWT-81 (imap-idle-missed-arrivals): every IDLE session's life is logged, so a
// stretch of missed INBOX arrivals leaves a record of whether a session was
// open, refreshing, or being ended by the server. Logging only; these tests pin
// the lines, one per outcome.

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// idleLogRun runs one idleOnce cycle and returns what it printed.
func idleLogRun(t *testing.T, signal chan struct{}, refresh time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink := newMWFakeSink()
	conn := &mwFakeConn{log: sink, signal: signal}
	open := func(context.Context, google.Account) (idleConn, error) { return conn, nil }

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	runErr := idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: (&mwClock{}).Sleep, IdleRefresh: refresh},
		mwAccount(1, "salvador@handsonconnect.org"), make(chan string, 1))
	os.Stdout = orig
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	if runErr != nil {
		t.Fatalf("idleOnce = %v", runErr)
	}
	return buf.String()
}

func TestIdleOnce_LogsOpenAndFired(t *testing.T) {
	sig := make(chan struct{}, 1)
	sig <- struct{}{}
	out := idleLogRun(t, sig, 25*time.Minute)
	for _, want := range []string{"watch: idle salvador@handsonconnect.org open", "watch: idle salvador@handsonconnect.org fired after"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestIdleOnce_LogsRefresh(t *testing.T) {
	out := idleLogRun(t, make(chan struct{}), 20*time.Millisecond)
	if !strings.Contains(out, "watch: idle salvador@handsonconnect.org refresh after") {
		t.Errorf("output %q lacks the refresh line", out)
	}
}

// A source that closes its channel with no update must say so. Since swb 556
// the production IMAP source does exactly this when the server ends the IDLE.
func TestIdleOnce_LogsASessionClosedWithoutNews(t *testing.T) {
	sig := make(chan struct{})
	close(sig)
	out := idleLogRun(t, sig, 25*time.Minute)
	if !strings.Contains(out, "watch: idle salvador@handsonconnect.org closed without news after") {
		t.Errorf("output %q lacks the closed-without-news line", out)
	}
}

// swb 556: the production source now closes its channel when the server ends
// the IDLE (Gmail's "connection closed"). Mail can land in the gap before the
// next IDLE starts, and IDLE reports only changes after it starts, so a dropped
// session asks for one catch-up pass. It also waits backoffMin before
// reopening, so a server that drops every session at once cannot make the
// watcher spin.
func TestIdleOnce_ADroppedSessionWakesACatchUpAndBacksOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sig := make(chan struct{})
	close(sig)
	sink := newMWFakeSink()
	conn := &mwFakeConn{log: sink, signal: sig}
	open := func(context.Context, google.Account) (idleConn, error) { return conn, nil }
	clock := &mwClock{}
	wake := make(chan string, 1)
	if err := idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: clock.Sleep, IdleRefresh: 25 * time.Minute},
		mwAccount(1, "sspataro@gmail.com"), wake); err != nil {
		t.Fatalf("idleOnce = %v", err)
	}
	select {
	case got := <-wake:
		if got != "sspataro@gmail.com" {
			t.Errorf("wake = %q, want the account", got)
		}
	default:
		t.Errorf("a dropped session asked for no catch-up pass: mail that landed in the gap waits for the sweep")
	}
	clock.mu.Lock()
	slept := append([]time.Duration(nil), clock.slept...)
	clock.mu.Unlock()
	if len(slept) != 1 || slept[0] != backoffMin {
		t.Errorf("slept %v, want one backoffMin (%v) before reopening", slept, backoffMin)
	}
}
