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

// The guard path (see watch.go): a source that closes its channel with no
// update must say so. The production IMAP source cannot reach it today.
func TestIdleOnce_LogsASessionClosedWithoutNews(t *testing.T) {
	sig := make(chan struct{})
	close(sig)
	out := idleLogRun(t, sig, 25*time.Minute)
	if !strings.Contains(out, "watch: idle salvador@handsonconnect.org closed without news after") {
		t.Errorf("output %q lacks the closed-without-news line", out)
	}
}
