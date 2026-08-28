package capture

// The sixth criteria type, `person` (SWT-17 SPEC §1), in its own file so it can
// be deleted in one move if it is being deferred — see the DECISION note below.
//
// GREENFIELD NOTE: this compile-FAILs today twice over — internal/capture/rules.go
// does not exist, and the declared `Message` has no participants field, so the
// criterion cannot be evaluated at all.
//
// DECISION REQUIRED (flagged, not assumed): `person` is IN SCOPE per the SPEC
// ("the pure evaluator and its six criteria types") and is the type that subsumes
// the dropped `projects.client_person_id` — "one rule row does the job the
// special-case column did". Evaluating it needs the thread's participants, which
// the declared Message does not carry. Two honest outcomes:
//
//	(a) Message grows `Participants []int64` (loaded from
//	    normalized_threads.participants) and this test passes;
//	(b) `person` is deferred — in which case DELETE this file AND make
//	    capture_rule_add refuse criteria_type='person' at insert time. A rule kind
//	    that is accepted by the tool, stored, listed by `opsctl capture-rules list`
//	    and silently matches nothing is the failure mode this repo has paid for
//	    three times (SWT-18's constant discriminator, the inert time floor, the
//	    single room column).
//
// What is NOT a third option: keeping the enum value and letting Evaluate ignore
// it. `participants` is '[]' for 16,959 of 16,985 threads, so a person rule
// matching nothing looks exactly like the data being empty, forever.

import "testing"

func TestEvaluate_PersonCriterion(t *testing.T) {
	const personID = int64(4242)
	rule := Rule{ID: 40, Project: "collaboratory", Kind: "person", Pattern: "4242", Priority: 50, Enabled: true}

	t.Run("matches when the person is a thread participant", func(t *testing.T) {
		// upworkcrm/sink.go:231 is the one sink that populates participants; its
		// thread keys are the SWT-19 shapes.
		msg := Message{
			ID: 1, Source: "upwork@pg-main", ThreadKey: "upwork_crm:eeee1111-0000-0000-0000-00000000cafe:room:room_5c4b3a2918",
			Sender: "Acme Corp", BodyText: "can you start Monday?",
			Participants: []int64{personID},
		}
		got := Evaluate(msg, []Rule{rule})
		assertMatched(t, got, rule.ID, "collaboratory")
		if got.ExternalKey != msg.ThreadKey {
			t.Errorf("Match.ExternalKey = %q, want the thread_key %q (§3: key_regex NULL on a non-body rule)",
				got.ExternalKey, msg.ThreadKey)
		}
	})

	t.Run("does not match another person on the thread", func(t *testing.T) {
		msg := Message{
			ID: 2, ThreadKey: "upwork_crm:eeee2222-0000-0000-0000-00000000beef:upwork",
			Participants: []int64{424, 42420},
		}
		// 424 and 42420 both CONTAIN "4242" as text but are different people —
		// the pattern is a people.id, so the comparison is on the id, not on the
		// rendered array.
		assertUnmatched(t, Evaluate(msg, []Rule{rule}), "person 4242 is not a participant; 424 and 42420 are other people")
	})

	t.Run("does not match a thread with no participants", func(t *testing.T) {
		// The state of 16,959 of 16,985 threads: the google/jira/slackweb sinks
		// hardcode '[]'. This must be a clean miss, not a panic.
		msg := Message{ID: 3, ThreadKey: "jira:treetopllc.jira.com:WEB-1204"}
		assertUnmatched(t, Evaluate(msg, []Rule{rule}), "participants is empty for every non-upwork thread today")
	})
}
