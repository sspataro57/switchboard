//go:build integration

package tools_test

// user-mail-read (swb 4683): mail_read_thread with require_general_mail:"true"
// (the user profile's pin) runs the attachment tools' SWT-21 judge on every
// message and withholds the rest. Reuses SWT-42's fixture, whose locality cases
// are already pinned against the attachment tools, so the two gates cannot
// disagree silently: every thread here is one of TestMailAttach_Integration_
// LocalityCasesFromPostgres's messages.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop the `if gated` judge call → LocalOnlyThreadWithheld, MixedThread;
//   - set threadKey/subject from a withheld row → LocalOnlyThreadWithheld;
//   - return the thread row's subject when gated → MixedThreadSubject…;
//   - honour the caller's limit when gated → GatedIgnoresLimit;
//   - gate without the pin → UnpinnedReadsAsBefore;
//   - drop the validator branch → PinValueValidated.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type mrOut struct {
	ThreadKey string `json:"thread_key"`
	Subject   string `json:"subject"`
	Messages  []struct {
		MessageID string `json:"message_id"`
		Direction string `json:"direction"`
		BodyText  string `json:"body_text"`
	} `json:"messages"`
	Withheld *int `json:"withheld_private"`
}

func mrRead(t *testing.T, ctx context.Context, raw json.RawMessage, err error) mrOut {
	t.Helper()
	if err != nil {
		t.Fatalf("mail_read_thread: %v", err)
	}
	var out mrOut
	maDecode(t, raw, &out)
	return out
}

func mrMIDs(o mrOut) []string {
	var ids []string
	for _, m := range o.Messages {
		ids = append(ids, m.MessageID)
	}
	return ids
}

func TestMailReadGate_Integration(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)
	gated := func(thread int64) mrOut {
		raw, err := maCall(ctx, ex, maHuman, "mail_read_thread", map[string]any{"thread_id": thread, "require_general_mail": "true"})
		return mrRead(t, ctx, raw, err)
	}

	t.Run("AnyFiledThreadShownWhole", func(t *testing.T) {
		// (a) inbound filed under `any` + (e) outbound on its thread.
		o := gated(fx.t1ID)
		if len(o.Messages) != 2 || o.Withheld == nil || *o.Withheld != 0 {
			t.Fatalf("T1 = %v withheld %v, want both messages and withheld_private 0", mrMIDs(o), o.Withheld)
		}
		if o.ThreadKey != fx.t1Key || !strings.Contains(o.Messages[0].BodyText, maBodyMarker) {
			t.Errorf("T1 = key %q body %q, want %q and the full body", o.ThreadKey, o.Messages[0].BodyText, fx.t1Key)
		}
	})

	t.Run("LocalOnlyThreadWithheld", func(t *testing.T) {
		o := gated(fx.local.thread)
		if len(o.Messages) != 0 || o.Withheld == nil || *o.Withheld != 1 {
			t.Fatalf("local_only thread = %v withheld %v, want nothing shown and 1 withheld", mrMIDs(o), o.Withheld)
		}
		if o.ThreadKey != "" || o.Subject != "" {
			t.Errorf("a fully withheld thread leaked key %q / subject %q", o.ThreadKey, o.Subject)
		}
	})

	t.Run("UnfiledOnUncleanMailboxWithheld", func(t *testing.T) {
		for _, m := range []maMsg{fx.unmatched, fx.unseen} {
			if o := gated(m.thread); len(o.Messages) != 0 || o.Withheld == nil || *o.Withheld != 1 {
				t.Errorf("thread of %s = %v withheld %v, want withheld", m.mid, mrMIDs(o), o.Withheld)
			}
		}
	})

	t.Run("MixedThread", func(t *testing.T) {
		// (g): g1 any-filed shown; g2 local_only and g3 (outbound folded over
		// a thread holding private inbound) withheld.
		o := gated(fx.t6ID)
		if len(o.Messages) != 1 || o.Messages[0].MessageID != fx.g1.mid || o.Withheld == nil || *o.Withheld != 2 {
			t.Fatalf("T6 = %v withheld %v, want only %s and 2 withheld", mrMIDs(o), o.Withheld, fx.g1.mid)
		}
		if o.ThreadKey != fx.t6Key {
			t.Errorf("T6 key = %q, want %q (shown, as the attachment tools show it)", o.ThreadKey, fx.t6Key)
		}
	})

	t.Run("MixedThreadSubjectIsTheShownMessagesOwn", func(t *testing.T) {
		// The thread row keeps its first ingested message's subject; make it a
		// private one and the gated read must not return it.
		if _, err := pool.Exec(ctx, `UPDATE normalized_threads SET subject='itest-mailread PRIVATE subject' WHERE id=$1`, fx.t6ID); err != nil {
			t.Fatalf("set thread subject: %v", err)
		}
		if o := gated(fx.t6ID); o.Subject != "itest-mailattach (g)" {
			t.Errorf("T6 subject = %q, want g1's own subject", o.Subject)
		}
	})

	t.Run("GatedIgnoresLimit", func(t *testing.T) {
		// limit:1 would cut T6 at g1; the count must still cover the thread.
		raw, err := maCall(ctx, ex, maHuman, "mail_read_thread", map[string]any{"thread_id": fx.t6ID, "limit": 1, "require_general_mail": "true"})
		if o := mrRead(t, ctx, raw, err); o.Withheld == nil || *o.Withheld != 2 {
			t.Errorf("gated limit:1 on T6 withheld %v, want 2: the caller's limit must be ignored when gated", o.Withheld)
		}
	})

	t.Run("CleanMailboxUnfiledShown", func(t *testing.T) {
		// (h) unmatched inbound on an O2-clean mailbox + (k) outbound on it.
		if o := gated(fx.h.thread); len(o.Messages) != 2 || o.Withheld == nil || *o.Withheld != 0 {
			t.Errorf("(h) thread = %v withheld %v, want both shown", mrMIDs(o), o.Withheld)
		}
	})

	t.Run("UnpinnedReadsAsBefore", func(t *testing.T) {
		raw, err := maCall(ctx, ex, maHuman, "mail_read_thread", map[string]any{"thread_id": fx.local.thread})
		o := mrRead(t, ctx, raw, err)
		if len(o.Messages) != 1 || o.Withheld != nil {
			t.Errorf("unpinned local_only thread = %v withheld %v, want the message and no withheld_private key", mrMIDs(o), o.Withheld)
		}
	})

	t.Run("PinValueValidated", func(t *testing.T) {
		if _, err := maCall(ctx, ex, maHuman, "mail_read_thread", map[string]any{"thread_id": fx.t1ID, "require_general_mail": "yes"}); err == nil {
			t.Error(`require_general_mail "yes" was accepted; want "true" or omitted`)
		}
	})
}
