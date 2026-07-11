package fleet_test

// Unit tests for the MQTT fleet contract (SPEC 03-mqtt-heartbeats, acceptance
// criteria 2 and 4; "The contract" normative section). ZERO network, ZERO
// Postgres — this is the pure payload/topic surface that steps 4/5 import.
//
// GREENFIELD NOTE: package internal/fleet does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented — the expected
// failure mode. Exported surface imposed here (SPEC's contract.go):
//
//   // State vocabulary. The first four are the ONLY live-publish states.
//   // StateDead is reserved for the LWT and is never emitted by Status.Marshal.
//   const (
//       StateIdle          = "idle"
//       StateWorking       = "working"
//       StateNeedsFeedback = "needs_feedback"
//       StateManual        = "manual"
//       StateDead          = "dead"
//   )
//   // Command verbs (CLAUDE.md's cmd set).
//   const (
//       ActionResume   = "resume"
//       ActionPause    = "pause"
//       ActionDispatch = "dispatch"
//   )
//   const (
//       StatusFilter      = "ops/workers/+/status" // mirror subscription
//       HeartbeatInterval = 60 * time.Second       // republish cadence
//   )
//
//   func StatusTopic(workerID string) string           // ops/workers/{workerID}/status
//   func CmdTopic(workerID string) string               // ops/workers/{workerID}/cmd
//   func ParseStatusTopic(topic string) (workerID string, err error)
//   func ClientFromWorkerID(workerID string) string     // segment up to first '.'
//   func ValidateWorkerID(workerID string) error        // empty / topic-meta chars -> error
//   func LWTPayload() []byte                             // exactly {"state":"dead"}
//
//   type Status struct {
//       State  string     // required; publish side restricted to the four live states
//       TaskID *int64     // optional; omitted from JSON when nil
//       TS     time.Time  // optional publisher clock; omitted from JSON when zero
//   }
//   // Marshal is the STRICT publish path: it rejects any State outside the four
//   // live states (empty, "zombie", and "dead" all error).
//   func (s Status) Marshal() ([]byte, error)
//   // ParseStatus is the LENIENT consume path: unknown JSON fields tolerated,
//   // missing optional fields ok, state NOT vocabulary-checked (stored verbatim);
//   // only syntactically invalid JSON errors.
//   func ParseStatus(data []byte) (Status, error)
//
//   type Cmd struct {
//       Action string
//       Args   json.RawMessage // optional; omitted from JSON when empty
//   }
//   func (c Cmd) Marshal() ([]byte, error)     // strict: Action must be a known verb
//   func ParseCmd(data []byte) (Cmd, error)     // lenient

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
)

func ptrInt64(v int64) *int64 { return &v }

// Topic builders produce exactly CLAUDE.md's literals (criterion 2).
func TestStatusTopic_And_CmdTopic(t *testing.T) {
	if got := fleet.StatusTopic("acme"); got != "ops/workers/acme/status" {
		t.Errorf("StatusTopic(acme) = %q, want ops/workers/acme/status", got)
	}
	if got := fleet.CmdTopic("acme"); got != "ops/workers/acme/cmd" {
		t.Errorf("CmdTopic(acme) = %q, want ops/workers/acme/cmd", got)
	}
	// Dotted (multi-console) worker id stays one topic level.
	if got := fleet.StatusTopic("acme.backend"); got != "ops/workers/acme.backend/status" {
		t.Errorf("StatusTopic(acme.backend) = %q, want ops/workers/acme.backend/status", got)
	}
}

// ParseStatusTopic extracts worker_id from a status topic and rejects anything
// that is not a concrete status topic (criterion 2).
func TestParseStatusTopic(t *testing.T) {
	cases := []struct {
		topic     string
		wantID    string
		wantError bool
	}{
		{"ops/workers/acme/status", "acme", false},
		{"ops/workers/acme.backend/status", "acme.backend", false},
		{"ops/workers/acme/cmd", "", true},          // cmd topic, not status
		{"ops/workers/+/status", "", true},          // filter, not a concrete id
		{"ops/workers/acme/status/extra", "", true}, // too many segments
		{"ops/workers//status", "", true},           // empty worker segment
		{"other/prefix/acme/status", "", true},      // wrong prefix
		{"", "", true},
	}
	for _, c := range cases {
		gotID, err := fleet.ParseStatusTopic(c.topic)
		if c.wantError {
			if err == nil {
				t.Errorf("ParseStatusTopic(%q): expected error, got id=%q", c.topic, gotID)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseStatusTopic(%q): unexpected error %v", c.topic, err)
			continue
		}
		if gotID != c.wantID {
			t.Errorf("ParseStatusTopic(%q) = %q, want %q", c.topic, gotID, c.wantID)
		}
	}
}

// client derives from worker_id as the segment before the first '.' — plain and
// dotted forms (criterion 2, unilateral dotted-subproject decision).
func TestClientFromWorkerID(t *testing.T) {
	cases := map[string]string{
		"acme":                "acme",
		"acme.backend":        "acme",
		"acme.backend.worker": "acme", // only the first '.' matters
	}
	for in, want := range cases {
		if got := fleet.ClientFromWorkerID(in); got != want {
			t.Errorf("ClientFromWorkerID(%q) = %q, want %q", in, got, want)
		}
	}
}

// ValidateWorkerID rejects empty ids and ids carrying MQTT topic-meta characters
// (a '/' would split topic levels; '+'/'#' are wildcards). Dotted ids are valid.
func TestValidateWorkerID(t *testing.T) {
	valid := []string{"acme", "acme.backend", "client-1"}
	for _, w := range valid {
		if err := fleet.ValidateWorkerID(w); err != nil {
			t.Errorf("ValidateWorkerID(%q): unexpected error %v", w, err)
		}
	}
	invalid := []string{"", "a/b", "a+b", "a#b"}
	for _, w := range invalid {
		if err := fleet.ValidateWorkerID(w); err == nil {
			t.Errorf("ValidateWorkerID(%q): expected error", w)
		}
	}
}

// The pinned live-publish vocabulary is exactly these four; dead is the reserved
// LWT-only state (criterion 2, "publish-side vocabulary").
func TestStateVocabulary(t *testing.T) {
	if fleet.StateIdle != "idle" || fleet.StateWorking != "working" ||
		fleet.StateNeedsFeedback != "needs_feedback" || fleet.StateManual != "manual" {
		t.Errorf("live state constants drifted: %q %q %q %q",
			fleet.StateIdle, fleet.StateWorking, fleet.StateNeedsFeedback, fleet.StateManual)
	}
	if fleet.StateDead != "dead" {
		t.Errorf("StateDead = %q, want dead", fleet.StateDead)
	}
}

// HeartbeatInterval is the pinned 60s cadence steps 4/5 inherit (Decisions).
func TestHeartbeatInterval(t *testing.T) {
	if fleet.HeartbeatInterval != 60*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 60s", fleet.HeartbeatInterval)
	}
}

// LWT payload is frozen and exact: {"state":"dead"}, no ts (criterion 4, the LWT
// is frozen at connect and a timestamp there would lie).
func TestLWTPayload_Exact(t *testing.T) {
	got := string(fleet.LWTPayload())
	if got != `{"state":"dead"}` {
		t.Errorf("LWTPayload() = %q, want {\"state\":\"dead\"}", got)
	}
	// It must parse leniently back to the dead state.
	s, err := fleet.ParseStatus(fleet.LWTPayload())
	if err != nil {
		t.Fatalf("ParseStatus(LWTPayload): %v", err)
	}
	if s.State != fleet.StateDead {
		t.Errorf("parsed LWT state = %q, want dead", s.State)
	}
}

// Status round-trips with all fields present (criterion 2).
func TestStatus_MarshalUnmarshalRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 11, 10, 30, 0, 0, time.UTC)
	in := fleet.Status{State: fleet.StateWorking, TaskID: ptrInt64(123), TS: ts}

	data, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := fleet.ParseStatus(data)
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if out.State != in.State {
		t.Errorf("state round-trip: got %q want %q", out.State, in.State)
	}
	if out.TaskID == nil || *out.TaskID != 123 {
		t.Errorf("task_id round-trip: got %v want 123", out.TaskID)
	}
	if !out.TS.Equal(ts) {
		t.Errorf("ts round-trip: got %v want %v", out.TS, ts)
	}
}

// Marshal omits the optional fields when unset: idle with no task_id / no ts
// serializes without those keys (criterion 2, "omitted when idle").
func TestStatus_MarshalOmitsOptionalFields(t *testing.T) {
	data, err := fleet.Status{State: fleet.StateIdle}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := m["task_id"]; ok {
		t.Errorf("task_id present when nil: %s", data)
	}
	if _, ok := m["ts"]; ok {
		t.Errorf("ts present when zero: %s", data)
	}
	if _, ok := m["state"]; !ok {
		t.Errorf("state missing: %s", data)
	}
}

// Strict publish: Marshal rejects states outside the four live ones — empty,
// an unknown "zombie", and the reserved "dead" all error (criterion 2 +
// "strict publish, lenient consume" decision).
func TestStatus_MarshalStrictRejectsInvalidState(t *testing.T) {
	for _, bad := range []string{"", "zombie", fleet.StateDead} {
		if _, err := (fleet.Status{State: bad}).Marshal(); err == nil {
			t.Errorf("Status{State:%q}.Marshal(): expected strict-publish error", bad)
		}
	}
}

// Lenient consume: unknown JSON fields are tolerated and missing optional fields
// are fine (criterion 2, "lenient consume").
func TestParseStatus_LenientUnknownAndMissingFields(t *testing.T) {
	// Unknown fields ignored; only required state present.
	s, err := fleet.ParseStatus([]byte(`{"state":"idle","unknown":true,"nested":{"a":1}}`))
	if err != nil {
		t.Fatalf("ParseStatus (unknown fields): %v", err)
	}
	if s.State != fleet.StateIdle {
		t.Errorf("state = %q, want idle", s.State)
	}
	if s.TaskID != nil {
		t.Errorf("task_id = %v, want nil (missing optional)", s.TaskID)
	}
	if !s.TS.IsZero() {
		t.Errorf("ts = %v, want zero (missing optional)", s.TS)
	}
}

// Lenient consume does NOT mean "accept anything": syntactically broken JSON
// errors so the mirror can count it (criterion 2 / criterion 3 malformed path).
func TestParseStatus_GarbageJSONErrors(t *testing.T) {
	if _, err := fleet.ParseStatus([]byte(`{not json`)); err == nil {
		t.Errorf("ParseStatus(garbage): expected error")
	}
}

// Cmd round-trips action + args (the envelope steps 4/5 build on).
func TestCmd_MarshalUnmarshalRoundTrip(t *testing.T) {
	in := fleet.Cmd{Action: fleet.ActionResume, Args: json.RawMessage(`{"task_id":5}`)}
	data, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := fleet.ParseCmd(data)
	if err != nil {
		t.Fatalf("ParseCmd: %v", err)
	}
	if out.Action != fleet.ActionResume {
		t.Errorf("action round-trip: got %q want %q", out.Action, fleet.ActionResume)
	}
	var args struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(out.Args, &args); err != nil {
		t.Fatalf("unmarshal round-tripped args %q: %v", out.Args, err)
	}
	if args.TaskID != 5 {
		t.Errorf("args.task_id round-trip: got %d want 5", args.TaskID)
	}
}

// Strict publish for commands: an action outside the verb set errors.
func TestCmd_MarshalStrictRejectsInvalidAction(t *testing.T) {
	for _, bad := range []string{"", "self-destruct"} {
		if _, err := (fleet.Cmd{Action: bad}).Marshal(); err == nil {
			t.Errorf("Cmd{Action:%q}.Marshal(): expected strict-publish error", bad)
		}
	}
}
