// Package upworkcrm is the Upwork CRM connector: it polls the upwork_crm
// database (read-only), lands rows raw-first in raw_source_items, then
// deterministically normalizes them into the canonical objects
// (SPEC 02-upwork-crm-connector / SWT-2).
package upworkcrm

import (
	"encoding/json"

	"github.com/sspataro57/switchboard/internal/connector/chash"
)

// ContentHash returns the lowercase-hex sha256 of the row's canonical JSON —
// delegated to the shared internal/connector/chash (the google connector is
// the second consumer). Kept as a package symbol so callers and tests are
// unchanged.
func ContentHash(raw json.RawMessage) (string, error) {
	return chash.ContentHash(raw)
}
