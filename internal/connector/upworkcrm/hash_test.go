package upworkcrm_test

// Unit tests for the content-hash contract (SPEC 02-upwork-crm-connector,
// acceptance criterion 1; supports idempotency in criteria 5 & 6). These run
// with ZERO network / ZERO Postgres.
//
// GREENFIELD NOTE: package internal/connector/upworkcrm does not exist yet, so
// this file compile-FAILs under `go test ./...`. That is the expected failure
// mode until the connector is implemented. Expected exported surface exercised
// here (the SPEC's hash.go):
//
//   // ContentHash returns the lowercase-hex sha256 of the row's CANONICAL JSON.
//   // Canonical = key-sorted re-encoding of the (jsonb) source row, so the hash
//   // is stable across key order and whitespace — matching Postgres jsonb text
//   // output. It is the value stored in raw_source_items.content_hash and the
//   // short-circuit key for idempotent upserts.
//   func ContentHash(raw json.RawMessage) (string, error)

import (
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

// Deterministic: the same logical row hashes to the same value every call.
func TestContentHash_Deterministic(t *testing.T) {
	raw := json.RawMessage(`{"id":"3f2a","body":"hello","is_draft":false}`)

	h1, err := upworkcrm.ContentHash(raw)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	h2, err := upworkcrm.ContentHash(raw)
	if err != nil {
		t.Fatalf("ContentHash (second call): %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
	// Sanity: a hex sha256 is 64 chars. Pins "sha256 hex" from the SPEC.
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (hex sha256): %q", len(h1), h1)
	}
}

// Stable across key order: canonical JSON means two encodings of the same
// logical row — different key order, different whitespace — hash identically.
// This is what makes the 24h-overlap re-read free (idempotent upserts).
func TestContentHash_StableAcrossKeyOrder(t *testing.T) {
	a := json.RawMessage(`{"id":"3f2a","body":"hello","is_draft":false}`)
	b := json.RawMessage(`{"is_draft":false,  "body":"hello",
		"id":"3f2a"}`)

	ha, err := upworkcrm.ContentHash(a)
	if err != nil {
		t.Fatalf("ContentHash(a): %v", err)
	}
	hb, err := upworkcrm.ContentHash(b)
	if err != nil {
		t.Fatalf("ContentHash(b): %v", err)
	}
	if ha != hb {
		t.Errorf("hash differs across key order / whitespace: %q vs %q (must be canonical)", ha, hb)
	}
}

// Differs on any field change: a single-field mutation must change the hash,
// otherwise criterion 6 (change handling) can never fire.
func TestContentHash_DiffersOnFieldChange(t *testing.T) {
	base := json.RawMessage(`{"id":"3f2a","body":"hello","is_draft":false}`)
	cases := map[string]json.RawMessage{
		"body changed":    json.RawMessage(`{"id":"3f2a","body":"HELLO","is_draft":false}`),
		"bool changed":    json.RawMessage(`{"id":"3f2a","body":"hello","is_draft":true}`),
		"field added":     json.RawMessage(`{"id":"3f2a","body":"hello","is_draft":false,"sender":"x"}`),
		"id changed":      json.RawMessage(`{"id":"9b1c","body":"hello","is_draft":false}`),
		"null vs missing": json.RawMessage(`{"id":"3f2a","body":"hello","is_draft":false,"subject":null}`),
	}

	baseHash, err := upworkcrm.ContentHash(base)
	if err != nil {
		t.Fatalf("ContentHash(base): %v", err)
	}
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			h, err := upworkcrm.ContentHash(changed)
			if err != nil {
				t.Fatalf("ContentHash: %v", err)
			}
			if h == baseHash {
				t.Errorf("hash unchanged after mutation %q; every field change must alter the hash", name)
			}
		})
	}
}
