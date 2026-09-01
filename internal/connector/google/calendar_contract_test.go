package google_test

// Unit tests for the calendar half of SWT-24
// (docs/tickets/calendar-availability_SPEC.md, acceptance criteria 7, 14, 15
// and 22). ZERO network to Google: the identity check runs against the shared
// httptest fake (fake_google_test.go), which is the whole point of criterion 21
// — "no live Google credential in any test".
//
// GREENFIELD NOTE (historical): CalendarScopes, PrimaryCalendarID,
// CalendarWindowPast and CalendarWindowFuture did not exist when this file was
// written (it compile-FAILed, the expected red) until the SWT-24
// implementation landed. Imposed surface:
//
//	// CalendarScopes is the consent this ticket asks for: calendar ONLY.
//	var CalendarScopes = []string{"https://www.googleapis.com/auth/calendar.readonly"}
//
//	// PrimaryCalendarID returns the primary calendar's id, which for a Google
//	// account IS the account address. add-calendar verifies the authorized
//	// identity with it, because addCmd's GetProfile needs a Gmail scope this
//	// consent deliberately does not request.
//	func (c *CalendarClient) PrimaryCalendarID(ctx context.Context) (string, error)
//
//	// The synced horizon, renamed from the unexported CalendarWindowPast /
//	// CalendarWindowFuture (ingest.go:19-22) so the availability wiring can refuse a
//	// window nothing was ever fetched for (criterion 7).
//	const CalendarWindowPast   = 30 * 24 * time.Hour
//	const CalendarWindowFuture = 90 * 24 * time.Hour

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// Criterion 14. Migration 0014 abandoned OAuth for mail because the restricted
// Gmail scopes need Google verification and a CASA assessment and can be blocked
// by a Workspace admin. Re-requesting them to fix calendars would drag that
// whole problem back in for no benefit — IMAP and SMTP already work. So the new
// scope set is calendar.readonly and nothing else.
func TestCalendarScopes_CalendarReadonlyOnly(t *testing.T) {
	want := "https://www.googleapis.com/auth/calendar.readonly"
	if len(google.CalendarScopes) != 1 || google.CalendarScopes[0] != want {
		t.Fatalf("CalendarScopes = %v, want exactly [%s]", google.CalendarScopes, want)
	}
	for _, s := range google.CalendarScopes {
		if strings.Contains(s, "gmail") {
			t.Errorf("CalendarScopes requests a Gmail scope (%s). Consent for calendars must not re-request "+
				"the restricted scopes migration 0014 abandoned OAuth to avoid", s)
		}
	}
	// The existing sets are untouched: mail keeps whatever it had, and nothing
	// about the live IMAP/SMTP path changes in this ticket.
	if len(google.Scopes) < 2 {
		t.Errorf("google.Scopes was narrowed to %v; this ticket adds a scope set, it does not edit the old ones",
			google.Scopes)
	}
}

// Criterion 15: the authorized identity is verified before anything is stored.
// Five accounts in one browser is exactly where the wrong one gets clicked, and
// the existing add command already refuses a mismatch via GetProfile — which
// cannot be used here, because it needs a Gmail scope.
func TestPrimaryCalendarID_ReturnsTheAuthorizedAccount(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()

	cc := google.NewCalendarClient(userHTTPClient(acctA), fg.url())
	got, err := cc.PrimaryCalendarID(ctx)
	if err != nil {
		t.Fatalf("PrimaryCalendarID: %v", err)
	}
	if got != acctA {
		t.Errorf("PrimaryCalendarID = %q, want %q (the primary calendar's id IS the account address)", got, acctA)
	}

	// A different token means a different identity: the check has to be able to
	// come back with the WRONG address, or add-calendar's abort can never fire.
	other := google.NewCalendarClient(userHTTPClient(acctB), fg.url())
	gotB, err := other.PrimaryCalendarID(ctx)
	if err != nil {
		t.Fatalf("PrimaryCalendarID (second account): %v", err)
	}
	if gotB == got {
		t.Errorf("PrimaryCalendarID returned %q for both accounts; identity rides on the authorized client, "+
			"so the two must differ or the mismatch abort is untestable and inert", gotB)
	}
}

func TestPrimaryCalendarID_ErrorsOnAnUnauthorizedResponse(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGoogle()
	defer fg.close()

	// No X-Fake-User: the fake cannot resolve an identity and answers 401,
	// exactly as Google answers a request whose token was revoked or never
	// carried the calendar scope.
	cc := google.NewCalendarClient(http.DefaultClient, fg.url())
	if id, err := cc.PrimaryCalendarID(ctx); err == nil {
		t.Errorf("PrimaryCalendarID returned %q with no authorized identity; storing a refresh token after an "+
			"unverified identity check is how the wrong account gets wired to a mailbox", id)
	}
}

// Criterion 7 (the constants half): the synced horizon stops being a private
// number. A caller asking propose_slots for a window 120 days out gets an empty
// busy set from a PERFECTLY fresh sync, because nothing was ever fetched there —
// the same fail-open on a second axis. The availability wiring refuses that
// window, and it must do so against THESE constants, not a re-spelled literal
// that drifts the first time the window changes.
func TestCalendarWindowConstantsAreExported(t *testing.T) {
	if google.CalendarWindowPast != 30*24*time.Hour {
		t.Errorf("CalendarWindowPast = %v, want 30d (the official sync recipe's initial window)", google.CalendarWindowPast)
	}
	if google.CalendarWindowFuture != 90*24*time.Hour {
		t.Errorf("CalendarWindowFuture = %v, want 90d", google.CalendarWindowFuture)
	}
}

// Criterion 22, structural. Invariant 4: an outbound calendar action needs a
// deliveries row and a policy tier, and `calendar` is still channel_not_live
// (matrix.go). The moment this client can PATCH, a future caller can book
// without either — so the ban is mechanical, not a comment. Checked here rather
// than in prose because the file is about to grow a new method.
func TestCalendarClient_IssuesNoWrites(t *testing.T) {
	src, err := os.ReadFile("calendar.go")
	if err != nil {
		t.Fatalf("read calendar.go: %v", err)
	}
	body := string(src)

	writeMethod := regexp.MustCompile(`http\.Method(Post|Put|Patch|Delete)|"(POST|PUT|PATCH|DELETE)"`)
	if m := writeMethod.FindAllString(body, -1); len(m) > 0 {
		t.Errorf("calendar.go issues %v against the Calendar API. This ticket READS calendars and writes "+
			"nothing to Google: an outbound calendar action needs a deliveries row and a policy tier, and "+
			"the calendar channel is still channel_not_live (invariant 4)", m)
	}
	// Control: a scan that matches nothing certifies nothing.
	if !strings.Contains(body, "http.MethodGet") {
		t.Errorf("calendar.go contains no http.MethodGet; the write-method scan has probably stopped matching " +
			"the file's actual request-building code")
	}
}
