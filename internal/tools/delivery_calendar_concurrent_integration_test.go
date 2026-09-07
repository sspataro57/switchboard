//go:build integration

package tools_test

// The concurrent-booking race (SWT-28, codex adversarial finding 1,
// 2026-09-07): before the fix, LoadBusy was an unlocked snapshot and the
// phase-1 transaction locked only its own delivery row — two different
// calendar deliveries over intersecting intervals could both see the slot
// free, both reserve distinct event ids, and both POST. The fix serializes
// allocation under a global pg_advisory_xact_lock spanning pre-flight +
// reserve, and makes an unconfirmed reservation LoadBusy-visible; this test
// is the parallel proof with two delivery ids over the SAME interval.
//
// Reuses delivery_calendar_integration_test.go's fixture (same package, same
// build tag, same cleanup pact).

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestCalendarBook_Integration_ConcurrentOverlappingBookingsBookExactlyOne(t *testing.T) {
	ctx := context.Background()
	f := newCalBookFixture(t, ctx)
	start, end := calBookBlock(18)

	d1 := f.draftCalendar(t, ctx, calBookWorker, start, end)
	d2 := f.draftCalendar(t, ctx, calBookWorker, start, end)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []int64{d1, d2} {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			_, errs[i] = f.book(ctx, calBookWorker, id)
		}(i, id)
	}
	wg.Wait()

	var booked, refused int
	for i, err := range errs {
		if err == nil {
			booked++
			continue
		}
		refused++
		if !strings.Contains(err.Error(), "conflicts with an existing busy interval") {
			t.Errorf("loser %d refused with %v, want the overlap refusal — the same wording a conflicting "+
				"propose_slots-era draft gets, because the reservation IS busy", i, err)
		}
	}
	if booked != 1 || refused != 1 {
		t.Fatalf("concurrent overlapping bookings: %d booked, %d refused (errs=%v), want exactly 1 and 1. "+
			"Two sends here is the double-book the advisory lock + LoadBusy-visible reservation exist to "+
			"prevent", booked, refused, errs)
	}
	if f.booker.calls != 1 {
		t.Errorf("the write route was called %d times for two concurrent overlapping bookings, want exactly 1",
			f.booker.calls)
	}

	// One row sent with its reservation, the other still approved and
	// retryable elsewhere/elsewhen.
	var sent, approved int
	for _, id := range []int64{d1, d2} {
		status, extID, _, _ := f.row(t, ctx, id)
		switch status {
		case "sent":
			sent++
			if !strings.HasPrefix(extID, "calendar:") {
				t.Errorf("winner %d has sent_external_id %q", id, extID)
			}
		case "approved":
			approved++
			if extID != "" {
				t.Errorf("loser %d reserved sent_external_id %q despite the refusal", id, extID)
			}
		default:
			t.Errorf("delivery %d ended as %q, want sent or approved", id, status)
		}
	}
	if sent != 1 || approved != 1 {
		t.Errorf("row states: %d sent, %d approved, want 1 and 1", sent, approved)
	}
}
