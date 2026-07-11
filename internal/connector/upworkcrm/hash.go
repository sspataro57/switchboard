// Package upworkcrm is the Upwork CRM connector: it polls the upwork_crm
// database (read-only), lands rows raw-first in raw_source_items, then
// deterministically normalizes them into the canonical objects
// (SPEC 02-upwork-crm-connector / SWT-2).
package upworkcrm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ContentHash returns the lowercase-hex sha256 of the row's canonical JSON.
// Canonical = key-sorted re-encoding (encoding/json marshals maps with sorted
// keys), so the hash is stable across key order and whitespace. It is the
// value stored in raw_source_items.content_hash and the short-circuit key for
// idempotent upserts.
func ContentHash(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("parse raw json: %w", err)
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("canonicalize raw json: %w", err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}
