// Package fleet is the MQTT fleet contract (SPEC 03-mqtt-heartbeats / SWT-3):
// topic shapes, payload types, and the client wrapper that workers (step 4),
// the orchestrator (step 5), and the fleetd mirror share. The types here ARE
// the contract — change them and every fleet participant changes.
package fleet

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// State vocabulary. The first four are the ONLY live-publish states; StateDead
// is reserved for the LWT and never emitted by Status.Marshal.
const (
	StateIdle          = "idle"
	StateWorking       = "working"
	StateNeedsFeedback = "needs_feedback"
	StateManual        = "manual"
	StateDead          = "dead"
)

// Command verbs (CLAUDE.md's cmd set).
const (
	ActionResume   = "resume"
	ActionPause    = "pause"
	ActionDispatch = "dispatch"
)

const (
	// StatusFilter is the mirror's subscription.
	StatusFilter = "ops/workers/+/status"
	// HeartbeatInterval is the republish cadence publishers must meet even when
	// state is unchanged; views flag rows stale at 3x this.
	HeartbeatInterval = 60 * time.Second
)

func StatusTopic(workerID string) string {
	return "ops/workers/" + workerID + "/status"
}

func CmdTopic(workerID string) string {
	return "ops/workers/" + workerID + "/cmd"
}

// ParseStatusTopic extracts the worker id from a concrete status topic.
// Filters, cmd topics, and malformed shapes are rejected.
func ParseStatusTopic(topic string) (string, error) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "ops" || parts[1] != "workers" || parts[3] != "status" {
		return "", fmt.Errorf("not a status topic: %q", topic)
	}
	workerID := parts[2]
	if err := ValidateWorkerID(workerID); err != nil {
		return "", fmt.Errorf("topic %q: %w", topic, err)
	}
	return workerID, nil
}

// ClientFromWorkerID derives the client column: the segment up to the first
// '.' (dotted worker ids are multi-console: {client}.{subproject}).
func ClientFromWorkerID(workerID string) string {
	if i := strings.IndexByte(workerID, '.'); i >= 0 {
		return workerID[:i]
	}
	return workerID
}

// ValidateWorkerID rejects empty ids and MQTT topic-meta characters ('/' would
// split topic levels; '+' and '#' are wildcards). Dots are valid.
func ValidateWorkerID(workerID string) error {
	if workerID == "" {
		return fmt.Errorf("worker id is empty")
	}
	if strings.ContainsAny(workerID, "/+#") {
		return fmt.Errorf("worker id %q contains MQTT topic characters", workerID)
	}
	return nil
}

// LWTPayload is the frozen last-will payload — no ts (an LWT is registered at
// connect time; a timestamp there would lie).
func LWTPayload() []byte {
	return []byte(`{"state":"dead"}`)
}

// Status is one heartbeat. TS is the publisher clock, informational only — the
// mirror stamps last_seen at receive time.
type Status struct {
	State  string    `json:"state"`
	TaskID *int64    `json:"task_id,omitempty"`
	TS     time.Time `json:"ts,omitzero"`
}

var liveStates = map[string]struct{}{
	StateIdle: {}, StateWorking: {}, StateNeedsFeedback: {}, StateManual: {},
}

// Marshal is the strict publish path: only the four live states pass ("dead"
// is LWT-only). A worker cannot emit garbage through the library.
func (s Status) Marshal() ([]byte, error) {
	if _, ok := liveStates[s.State]; !ok {
		return nil, fmt.Errorf("state %q is not publishable (live states: idle|working|needs_feedback|manual)", s.State)
	}
	out, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal status: %w", err)
	}
	return out, nil
}

// ParseStatus is the lenient consume path: unknown fields tolerated, optional
// fields may be missing, state is NOT vocabulary-checked (the mirror stores
// violations verbatim so they stay visible). Only broken JSON errors.
func ParseStatus(data []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("parse status payload: %w", err)
	}
	return s, nil
}

// Cmd is the command envelope on ops/workers/{worker_id}/cmd. Args are
// action-specific; steps 4/5 pin their schemas.
type Cmd struct {
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args,omitempty"`
}

var actions = map[string]struct{}{
	ActionResume: {}, ActionPause: {}, ActionDispatch: {},
}

// Marshal is strict: the action must be a known verb.
func (c Cmd) Marshal() ([]byte, error) {
	if _, ok := actions[c.Action]; !ok {
		return nil, fmt.Errorf("action %q is not a known command verb (resume|pause|dispatch)", c.Action)
	}
	out, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal cmd: %w", err)
	}
	return out, nil
}

// ParseCmd is lenient (mirror of ParseStatus).
func ParseCmd(data []byte) (Cmd, error) {
	var c Cmd
	if err := json.Unmarshal(data, &c); err != nil {
		return Cmd{}, fmt.Errorf("parse cmd payload: %w", err)
	}
	return c, nil
}
