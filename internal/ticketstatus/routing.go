package ticketstatus

// Routing and freshness for the candidate-driven lookup (SWT-32 criteria 14,
// 15, 17). RouteLookup is pure and takes the KEY and the accounts and nothing
// else — the ref's URL COLUMN is not a parameter, which is criterion 15's
// SSRF guarantee made structural: that column is agent-facing free text
// written by link_external_ref, and a router that read it would let a crafted
// ref aim a stored API token at a host of the caller's choosing (D18).

import (
	"os"
	"regexp"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/jira"
)

// DefaultLookupTTL bounds how often one candidate is re-fetched (D20): the
// difference between 36 GETs a day and 3,456.
const DefaultLookupTTL = time.Hour

// issueKeyPattern is the shape of a routable ticket key: an uppercase project
// prefix (letters and digits, starting with a letter), a dash, and an
// uppercase-alphanumeric id. external_key is written by link_external_ref — an
// MCP agent-facing tool with free-text arguments — so anything the router
// cannot READ (lowercase, spaces, path characters, an empty id) fails closed
// as `unpolled`, before any HTTP request exists to be aimed.
var issueKeyPattern = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-[A-Z0-9]+$`)

// RouteLookup picks the lookup account whose declared scopes claim key's
// project prefix (D18). outcome is one of "routed" | "unpolled" | "ambiguous" —
// the counter names criterion 43 prints. A refusal hands back the zero Account:
// nothing to fetch with.
func RouteLookup(key string, accounts []jira.Account) (jira.Account, string) {
	m := issueKeyPattern.FindStringSubmatch(key)
	if m == nil {
		return jira.Account{}, "unpolled"
	}
	prefix := m[1]

	var claimed []jira.Account
	for _, a := range accounts {
		for _, p := range a.Projects {
			if p == prefix {
				claimed = append(claimed, a)
				break
			}
		}
	}
	switch len(claimed) {
	case 0:
		return jira.Account{}, "unpolled"
	case 1:
		return claimed[0], "routed"
	default:
		// The multi-match refusal precedent: refusing is reversible; guessing
		// spends a token against a site nobody chose.
		return jira.Account{}, "ambiguous"
	}
}

// LookupTTL reads TICKET_LOOKUP_TTL (a Go duration), defaulting to
// DefaultLookupTTL. Anything unparseable or non-positive falls back — capture's
// ObserveHorizon/RulesHorizon shape, for its recorded reason: "3600" is the
// realistic typo, it is not a Go duration, and read as nanoseconds it would
// make every candidate permanently stale.
func LookupTTL() time.Duration {
	raw := os.Getenv("TICKET_LOOKUP_TTL")
	if raw == "" {
		return DefaultLookupTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultLookupTTL
	}
	return d
}
