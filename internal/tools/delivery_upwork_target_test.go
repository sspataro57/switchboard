package tools_test

// Unit tests for draft_delivery's upwork_chat target_ref validation
// (SWT-19 acceptance criteria 7 and 8). ZERO network, ZERO Postgres: every
// assertion here stops at the executor's VALIDATE stage, which runs before any
// handler dereferences the pool — the tools_unit_test.go idiom, with a nil
// *pgxpool.Pool.
//
// Why this validation exists NOW rather than later, in two facts that point the
// same way:
//
//   - Production has ZERO upwork_chat deliveries and has never had one, so
//     tightening the rule costs nothing today.
//   - Since SWT-18 the matcher binds by exact target_ref, and since SWT-19 it
//     excludes any target it cannot PARSE (SPEC §4). A malformed target_ref is
//     therefore PERMANENTLY unconfirmable where the pre-SWT-18 LIKE was
//     forgiving — and it fails silently, as a delivery that simply never closes
//     its loop. draft_delivery previously checked only that the string was
//     non-empty (delivery.go:107-108), unlike slack_reply two lines below it,
//     which parses. IK names that gap as the SWT-13 canonicalisation landmine's
//     fourth instance.
//
// GREENFIELD NOTE: these fail today because validateDraftDelivery does not call
// upworkcrm.ParseThreadKey yet — a malformed upwork target is accepted at
// validate and the call proceeds to the handler (which, with a nil pool, panics
// or errors for the wrong reason). Expected red state.

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

func upworkTargetExecutor(t *testing.T) *executor.Executor {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

func draftUpworkTarget(t *testing.T, ex *executor.Executor, target string) error {
	t.Helper()
	_, err := ex.Execute(context.Background(), executor.Call{
		Tool: "draft_delivery", Actor: "unit",
		Args: []byte(`{"task_id":1,"channel":"upwork_chat","body":"thanks, will do","target_ref":` + quoteJSON(target) + `}`),
	})
	return err
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Criterion 7, first half: a target_ref ParseThreadKey rejects is refused at
// validate, and the error NAMES the target. "invalid target_ref" alone sends the
// reader hunting; the whole point of the parser's per-part errors is that this
// message can say which part is wrong.
func TestValidate_UpworkChatRejectsUnparseableTargetRef(t *testing.T) {
	ex := upworkTargetExecutor(t)

	cases := []struct {
		name, target string
	}{
		{"a slack target", "https://app.slack.com/client/T1/C1"},
		{"wrong provider prefix", "upwork:11111111-1111-1111-1111-111111111111:upwork"},
		{"only two segments", "upwork_crm:11111111-1111-1111-1111-111111111111"},
		{"empty client id", "upwork_crm::upwork"},
		{"empty channel segment", "upwork_crm:11111111-1111-1111-1111-111111111111:"},
		{"roomed but the room id is empty", "upwork_crm:11111111-1111-1111-1111-111111111111:room:"},
		{"four segments whose third is not `room`", "upwork_crm:11111111-1111-1111-1111-111111111111:rooms:room_1a2b"},
		{"a bare client id", "11111111-1111-1111-1111-111111111111"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := draftUpworkTarget(t, ex, tc.target)
			if err == nil {
				t.Fatalf("draft_delivery accepted upwork_chat target_ref %q. Since SWT-18 the matcher binds by "+
					"exact target_ref and since SWT-19 it excludes what it cannot parse, so this delivery could "+
					"never be confirmed — silently, forever, with sent_external_id NULL", tc.target)
			}
			msg := err.Error()
			if strings.Contains(msg, "unknown tool") {
				t.Fatalf("draft_delivery is not registered: %q", msg)
			}
			if strings.Contains(msg, "denied by policy") {
				t.Fatalf("got a policy denial %q; the refusal must happen at VALIDATE, before any policy or audit "+
					"write — a malformed target is not a permissions question", msg)
			}
			if !strings.Contains(msg, "validate") {
				t.Errorf("error = %q, want a validate-stage failure", msg)
			}
			if !strings.Contains(msg, "target_ref") {
				t.Errorf("error = %q, want it to mention target_ref", msg)
			}
			if !strings.Contains(msg, tc.target) {
				t.Errorf("error = %q, want it to NAME the offending target %q (criterion 7)", msg, tc.target)
			}
		})
	}
}

// Criterion 7, second half: BOTH shapes are accepted. The unroomed form is not a
// legacy wart to be phased out — 2,009 of the ops corpus's messages live on it,
// and §4's mismatch-only-excludes rule is what keeps an unroomed target
// confirmable by a roomed message. Refusing it here would make the legacy
// conversation undeliverable.
//
// "Accepted" is asserted as "does not fail at VALIDATE": the handler needs a
// pool, so the call cannot succeed here, and a handler-stage failure is the
// positive result.
func TestValidate_UpworkChatAcceptsBothKeyShapes(t *testing.T) {
	ex := upworkTargetExecutor(t)
	const client = "e2ef9b65-9813-4d79-ac10-0e1813f788ff"

	shapes := []struct {
		name, target string
	}{
		{"roomed", "upwork_crm:" + client + ":room:room_1a2b3c4d5e"},
		{"unroomed legacy", "upwork_crm:" + client + ":upwork"},
		{"roomed, room id containing a colon", "upwork_crm:" + client + ":room:room_1a2b:extra"},
	}

	// Fixture guard: the two shapes must be different strings, or "both are
	// accepted" is one assertion made twice.
	if shapes[0].target == shapes[1].target {
		t.Fatalf("fixture invalid: the roomed and unroomed targets are the same string")
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			err := mustNotPanic(t, func() error { return draftUpworkTarget(t, ex, sh.target) })
			if err != nil && strings.Contains(err.Error(), "validate") {
				t.Errorf("draft_delivery refused a WELL-FORMED %s target %q at validate: %v", sh.name, sh.target, err)
			}
		})
	}
}

// Criterion 8's pin. The four integration fixtures that wrote
// `upwork_crm:itest-*:chat` are updated to canonical keys in this change; this
// test is what stops the old spelling drifting back in.
//
// READ THIS BEFORE "FIXING" THE TEST: the SPEC's criterion 8 asks for a test
// asserting "the old spelling is now refused", and `upwork_crm:itest-del:chat`
// is NOT refused — it is a well-formed UNROOMED key whose channel happens to be
// `chat`, and §2's parse rule ("3 segments, third non-empty -> unroomed with
// that channel") admits it deliberately, because the channel segment carries a
// source column whose values are not ours to enumerate. Refusing it would mean
// hardcoding `upwork` as the only legal channel, which is the "magic literal in
// the third segment" the `room:` tag exists to avoid.
//
// So the pin is on the class of target that genuinely IS now refused (above),
// plus this: the fixtures no longer carry a channel the source has never
// emitted. `chat` was fabricated — every row in the source db has
// channel='upwork' — and a fixture built on a value production never produces is
// how SWT-18 came to prove "room scoping" with `chat` and `room-b`.
func TestValidate_UpworkChatFixtureSpellingIsCanonical(t *testing.T) {
	ex := upworkTargetExecutor(t)

	// The spelling the four fixtures used before SWT-19.
	const old = "upwork_crm:itest-del:chat"
	// What they use now: the channel the source actually emits.
	const canonical = "upwork_crm:itest-del:upwork"

	if old == canonical {
		t.Fatalf("fixture invalid: the old and canonical spellings are the same string")
	}

	// Documented, not asserted as a refusal: the old spelling still parses. If
	// this ever starts failing, someone has restricted the channel segment —
	// which is a real decision, but it re-keys nothing and must be made
	// deliberately, not discovered here.
	if err := mustNotPanic(t, func() error { return draftUpworkTarget(t, ex, old) }); err != nil && strings.Contains(err.Error(), "validate") {
		t.Logf("NOTE: %q is now refused at validate (%v). That is a stricter rule than SPEC §2 describes; "+
			"if it is intended, say so in threadkey.go and update criterion 2's rejection list", old, err)
	}

	if err := mustNotPanic(t, func() error { return draftUpworkTarget(t, ex, canonical) }); err != nil && strings.Contains(err.Error(), "validate") {
		t.Errorf("the canonical fixture spelling %q was refused at validate: %v", canonical, err)
	}
}

// mustNotPanic runs fn, converting a panic from the handler stage (nil pool)
// into a non-validate error. Only the validate stage is under test here; the
// handler needs a database and is covered by the integration suite.
func mustNotPanic(t *testing.T, fn func() error) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			err = nil // reached the handler => validate passed, which is the result under test
		}
	}()
	return fn()
}
