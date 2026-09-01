package upworkcrm_test

// SWT-20 criterion 14's structural half, and verification-protocol step 6.
//
// Plain unit test — no build tag, no database — so it runs on every `go test
// ./...` pass, in the mould of keyspelling_test.go and
// internal/textmatch/callsites_test.go.
//
// WHAT IT PINS, and why a behavioural test alone is not enough: criterion 14
// says the candidate `SELECT ... FOR UPDATE` "is reached only with an explicit
// id set". The lock-contention test in shortlist_integration_test.go proves the
// EFFECT, but it needs Postgres and it skips silently without DATABASE_URL —
// and the shape it protects (shortlist, THEN lock) is exactly the kind of thing
// a later refactor collapses back into one query while every behavioural test
// still passes on a database with two rows in it.
//
// It also enforces the SPEC's least glamorous requirement: the block comment at
// sink.go:410-416, which today states in prose that the candidate set is "every
// upwork delivery never confirmed, EVER — across all clients", must be REWRITTEN
// rather than left standing next to code that falsifies it. IK: "a comment can
// be a defect" — SWT-21 shipped two comments stating the opposite of their code,
// and in a boundary file the comment is what the next session trusts.
//
// GREENFIELD NOTE: red today. sink.go still selects every unconfirmed upwork row
// FOR UPDATE with no id set and no client predicate, and still carries the
// "across all clients" paragraph.
//
// Deliberately NOT asserted: the exact SQL text, the parameter numbering, or
// whether the shortlist is one statement or two. Only the three facts criterion
// 14 and §6 actually require.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The raw-literal scanner, same idea as keyspelling_test.go's: whole backtick
// literals, so a multi-line SQL block whose clauses sit on different lines is
// still seen as one unit.
var sinkRawStringLit = regexp.MustCompile("(?s)`[^`]*`")

// commentFold collapses newlines, comment slashes and runs of whitespace so a
// claim wrapped across two comment lines is still one phrase.
var commentFold = regexp.MustCompile(`(?m)\s*(//)?\s+`)

func readSink(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("sink.go")
	if err != nil {
		t.Fatalf("read sink.go: %v", err)
	}
	return string(b)
}

// Criterion 14, structurally: every `FOR UPDATE` in this file locks an EXPLICIT
// id set.
//
// The cost this removes is O(outbound x unresolved): today one outbound message
// for any client locks every unresolved upwork delivery of every client, and the
// reconciler never removes a row from that set (it annotates, it does not
// resolve), so a single stuck row of client B blocks client A's connector run
// forever. Zero rows today — which is why it is fixable now for free.
func TestConfirmUpworkDelivery_LocksAnExplicitIDSet(t *testing.T) {
	src := readSink(t)

	// Positive controls: if these stop matching, the scan proves nothing.
	if !strings.Contains(src, "confirmUpworkDelivery") {
		t.Fatalf("sink.go no longer declares confirmUpworkDelivery; this scan has stopped checking anything")
	}
	if !strings.Contains(src, "FOR UPDATE") {
		t.Fatalf("sink.go contains no FOR UPDATE at all. Either the matcher stopped locking its candidates — " +
			"which would let two concurrent connector runs stamp the same row — or this scan is looking at " +
			"the wrong file")
	}

	locking := 0
	for _, lit := range sinkRawStringLit.FindAllString(src, -1) {
		if !strings.Contains(lit, "FOR UPDATE") {
			continue
		}
		locking++
		if !strings.Contains(lit, "= ANY(") {
			t.Errorf("sink.go locks candidates without an explicit id set:\n\t%s\n"+
				"Criterion 14: the FOR UPDATE select must be reached only with the ids the SHORTLIST returned. "+
				"Locking by predicate alone re-creates the O(outbound x unresolved) scan this ticket removes, "+
				"and one stuck row of another client blocks every run", firstSinkLines(lit, 6))
		}
		if !strings.Contains(lit, "ORDER BY id DESC") {
			t.Errorf("sink.go's locking select dropped `ORDER BY id DESC`:\n\t%s\n"+
				"Two concurrent connector runs must acquire row locks in the SAME order or they deadlock "+
				"instead of blocking — and that still holds for overlapping SUBSETS, which is what the "+
				"shortlist now produces (SPEC §6 step 2)", firstSinkLines(lit, 6))
		}
	}
	if locking == 0 {
		t.Errorf("no raw SQL literal in sink.go contains FOR UPDATE, yet the file mentions it — the lock has " +
			"probably moved into a built string, where this scan cannot see it")
	}
}

// Criterion 14's other half: the shortlist exists, and it narrows by the
// persisted client identity.
//
// `target_client_ref` is the ONLY predicate that may appear here. It is the only
// one IMPLIED BY SameConversation (see
// TestSameConversation_DifferentClientsNeverMatch_ShortlistPremise); the room
// clause is the rule itself, and spelling it in SQL would be the second-spelling
// landmine this repo has paid for four times (SPEC D3).
func TestConfirmUpworkDelivery_ShortlistsByTargetClientRef(t *testing.T) {
	src := readSink(t)

	if !strings.Contains(src, "target_client_ref") {
		t.Errorf("sink.go never mentions target_client_ref. Criterion 14: the matcher shortlists candidates " +
			"with `target_client_ref = $1` (the partial index deliveries_upwork_unconfirmed_idx) BEFORE it " +
			"locks anything, and returns having locked nothing when the shortlist is empty — the common case " +
			"once the tier is live")
	}
	// The room must NOT reach SQL. D3: any shortlist predicate has to be implied
	// by SameConversation, and "not (both roomed and rooms differ)" is the rule,
	// not an implication of it.
	for _, banned := range []string{"target_room_ref", "room_id =", "room_id="} {
		if strings.Contains(src, banned) {
			t.Errorf("sink.go's SQL mentions %q. The room is decided in GO, by SameConversation, over the "+
				"shortlisted rows — a room predicate in SQL is a second spelling of the candidate rule and "+
				"there is deliberately no room column (SPEC D3)", banned)
		}
	}
}

// Verification protocol step 6: the comment must stop contradicting the code.
//
// The paragraph at sink.go:410-416 is the honest description of TODAY's
// behaviour and becomes false the moment the shortlist lands. Leaving it is
// worse than never having written it: the next session reads it, believes the
// candidate set is client-wide, and reasons about a matcher that no longer
// exists.
func TestConfirmUpworkDelivery_CommentDoesNotOutliveItsCode(t *testing.T) {
	src := readSink(t)

	if !strings.Contains(src, "confirmUpworkDelivery") {
		t.Fatalf("sink.go no longer declares confirmUpworkDelivery; this scan has stopped checking anything")
	}
	// The claim is wrapped across two comment lines today ("... EVER \u2014 across" /
	// "// all clients, locked FOR UPDATE ..."), so the leading slashes and the
	// line breaks are folded away before looking for it. A scan that only sees
	// the phrase on one line would certify the very paragraph it exists to catch.
	flat := strings.ToLower(commentFold.ReplaceAllString(src, " "))
	if strings.Contains(flat, "across all clients") {
		t.Errorf("sink.go still says the candidate set is \"across all clients\". After the shortlist it is " +
			"scoped to the message's client, so that paragraph is now a defect in prose — IK: 'a comment can " +
			"be a defect', and in a boundary file the comment is what the next session trusts. Rewrite it: " +
			"say that the shortlist narrows by the persisted client identity, that the CHECK constraint is " +
			"what makes that sound, and that the narrowing can only ever cause a MISS (which the reconciler " +
			"surfaces), never a wrong stamp")
	}
}

func firstSinkLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n\t")
}
