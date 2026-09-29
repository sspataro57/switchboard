package google_test

// swb 556 / SWT-81: when the server ends an IDLE early ("imap: connection
// closed"), Idle must close its channel so the watcher reopens at once. It
// used to keep the channel open until the caller's 25-minute refresh, so the
// mailbox was deaf until then and INBOX arrivals waited for the 10-minute
// reconcile sweep: Grady's mail of 29 Sep 14:21:46Z sat 8 minutes behind a
// session Gmail had closed 11 s earlier.

import (
	"context"
	"testing"
	"time"
)

func TestIMAPClientSource_IdleClosesItsChannelWhenTheServerEndsIt(t *testing.T) {
	srv := newFakeIMAPServer(t, msxGmailCaps)
	srv.acceptLogin = true
	srv.dropOnIdle = true
	src := msxPasswordSource(t, srv, "sspataro@gmail.com", "app-password")
	defer func() { _ = src.Close() }()

	// The caller's context is the 25-minute refresh in production; here it is
	// far longer than the assertion's wait, so only an early end can close ch.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ch, err := src.Idle(ctx, "INBOX")
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("Idle signalled news; the server sent none, it hung up")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the server ended the IDLE but the channel stayed open: the watcher would sit on a dead " +
			"session until its refresh, and new mail would wait for the reconcile sweep")
	}
}

// The ordinary path is unchanged: with the server holding the IDLE open,
// the channel stays open until the caller's context ends.
func TestIMAPClientSource_IdleStaysOpenWhileTheServerHoldsIt(t *testing.T) {
	srv := newFakeIMAPServer(t, msxGmailCaps)
	srv.acceptLogin = true
	src := msxPasswordSource(t, srv, "sspataro@gmail.com", "app-password")
	defer func() { _ = src.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	ch, err := src.Idle(ctx, "INBOX")
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	select {
	case <-ch:
		if ctx.Err() == nil {
			t.Fatalf("the channel closed while the server still held the IDLE")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the channel never closed after the caller's context ended")
	}
}
