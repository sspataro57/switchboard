package jira

// IssueFacts reads the two facts SWT-32 turns on out of a STORED raw issue —
// the status category and the assignee — and nothing else. Pure: a function of
// the bytes the poller (or the candidate-driven lookup) already wrote, which is
// what makes the reconciler's decision reproducible from raw_source_items alone
// (D19).
//
// The Known booleans separate absent-because-impossible from
// absent-because-pending (D14): `"assignee": null` is a POSITIVE statement of
// unassignment (the key is present), while a fields object with no assignee key
// at all is missing evidence — the two are distinguishable in the JSON, so this
// reader distinguishes them, which is why fields is probed as a map rather than
// unmarshalled into a struct with a pointer field (a *user field reads both
// shapes as nil).

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Facts are the only two things this ticket reads out of an issue.
type Facts struct {
	StatusCategory string // fields.status.statusCategory.key — the discriminator (D2)
	StatusName     string // fields.status.name — DIAGNOSTIC ONLY; nothing branches on it
	StatusKnown    bool
	Assignee       string // fields.assignee.accountId — the identity D12 compares
	AssigneeKnown  bool
}

// IssueFacts parses one stored issue snapshot. Malformed bytes are an ERROR,
// not a zero Facts: an unreadable FACT is a counted no-op on one ref, while
// bytes that will not parse mean the stored row is corrupt and the operator
// should hear about it.
func IssueFacts(raw json.RawMessage) (Facts, error) {
	var f Facts

	var doc struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return f, fmt.Errorf("parse stored issue: %w", err)
	}

	// The status half. Any gap — no status key, a null status, no
	// statusCategory, a category with no key — is StatusKnown=false with an
	// EMPTY category: an invented value would fail migration 0023's CHECK at
	// insert time instead of being refused here, and a category the reader made
	// up is worse than none.
	if rawStatus, present := doc.Fields["status"]; present {
		var st struct {
			Name           string `json:"name"`
			StatusCategory *struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		}
		if err := json.Unmarshal(rawStatus, &st); err != nil {
			return f, fmt.Errorf("parse fields.status: %w", err)
		}
		// Only Jira's three navigable category keys are facts; anything else —
		// including the real fourth key `undefined` ("No Category", id 1) — is
		// an evidence gap, NOT a value. Migration 0023's CHECK admits exactly
		// these three, so an unrecognised key passed through would abort the
		// whole pass at INSERT time, every tick, at the same ref (go-reviewer
		// F1, 2026-09-09).
		key := ""
		if st.StatusCategory != nil {
			key = st.StatusCategory.Key
		}
		if key == "new" || key == "indeterminate" || key == "done" {
			f.StatusKnown = true
			f.StatusCategory = key
			f.StatusName = st.Name
		}
	}

	// The assignee half, independent of the status half. The comparison D12
	// makes is on accountId ONLY — a deleted/anonymised user serialises with no
	// accountId, and no id is invented for it.
	if rawAssignee, present := doc.Fields["assignee"]; present {
		f.AssigneeKnown = true
		if !bytes.Equal(bytes.TrimSpace(rawAssignee), []byte("null")) {
			var u struct {
				AccountID string `json:"accountId"`
			}
			if err := json.Unmarshal(rawAssignee, &u); err != nil {
				return f, fmt.Errorf("parse fields.assignee: %w", err)
			}
			f.Assignee = u.AccountID
		}
	}
	return f, nil
}
