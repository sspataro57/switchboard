// Package chash is the shared canonical-JSON content hash every connector
// stores in raw_source_items.content_hash (lifted from the upworkcrm
// connector when the google connector became its second consumer).
package chash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ContentHash returns the lowercase-hex sha256 of the row's canonical JSON
// (key-sorted re-encoding), stable across key order and whitespace.
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
