package classify

// The route lane (SWT-40 Part B, docs/tickets/inquiry-promote_SPEC.md B-D3,
// B-D4, B-D8): which of an account's CLOSED candidate projects does a message
// the capture rules left unmatched belong to?
//
// A FOURTH lane with a THIRD contract, argued in the SPEC rather than in this
// file: residue's contract is pinned equal to personal's (SWT-23), so folding
// routing into it would break that guard, and the inquiry lane needs the
// project context an unmatched message lacks.
//
// The lane RECORDS verdicts and decides nothing. The applied decision is a
// mode='route' capture_decisions row that internal/capture/route.go writes
// under capture's lock; this package never writes capture_decisions.
//
// Locality (B-D8): every row this lane's inbox yields has an 'unmatched' latest
// decision, so it carries Attribution = AttrUnmatched and ClassOf restricts it
// — the residue lane's mechanism, restated rather than inherited. The prompt
// carries only the message and the account's candidate rows: no thread
// neighbours, no links. cmd/classify and cmd/pipelined build the router with
// general = nil, so there is no hosted client to fall back to.
//
// The model answers with an INDEX into the numbered candidate list, resolved in
// Go (ResolveCandidate, ResolveLink's shape), and quotes EVIDENCE. Its own
// certainty is never asked for: qwen3:8b returns a constant for it (runbook
// criterion 18). What replaces it is Grounded, decided here at classify time,
// the way ResolveLink resolves link_url at classify time: route_apply reads the
// recorded bit and never re-reads a body.

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// RoutePromptVersion stamps every route ai_runs.input, distinct from the other
// lanes' stamps. route-v2 (2026-09-13, SPEC B-D4 amendment): the prompt now
// says evidence comes from the subject or body only, two words and 8
// characters at least — the rule Grounded enforces.
const RoutePromptVersion = "route-v2"

// RouteSchemaName is the route contract's structured-output name.
const RouteSchemaName = "route_verdict"

// RouteVerdictSchema is B-D4's contract: three required fields and no fourth.
// project_index is 1-based into the numbered candidate list, and null is how
// the model says "none of these" — ordinary output, not a failure.
var RouteVerdictSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["project_index", "evidence", "reason"],
  "properties": {
    "project_index": {"type": ["integer", "null"]},
    "evidence": {"type": "string"},
    "reason": {"type": "string"}
  }
}`)

// RouteContract is the route lane's own contract (B-D3). The labelled set's
// labels are project slugs (multi-class), so there is no single positive
// token; DecisionKey names the grounding bit, the one boolean a report can
// count as "a choice the application accepted".
var RouteContract = Contract{
	SchemaName:    RouteSchemaName,
	Schema:        RouteVerdictSchema,
	DecisionKey:   "grounded",
	CategoryKey:   "project_slug",
	PositiveLabel: "",
}

// RouteSystemPrompt is ONE prompt for every account and every sender (per-sender
// prompts are rules in a costume). It names no project, no client and no
// sender: the candidates arrive in the USER half, per message, from the
// account's source_account_projects rows, so a project name here would be a
// rule that outlives the row that justified it.
const RouteSystemPrompt = `You route one message to the project it belongs to.

The message arrived on a mailbox that serves a small, fixed set of projects. The
numbered list after the message is that complete set of candidates: each line
gives the project's short name, its full name, its client, and a description of
the work it covers. Messages may be in English or Spanish; treat both
identically.

Choose a candidate ONLY when the message itself says which project it is about:
the project or product is named, the work described is unmistakably that
project's, or a ticket or system named in the message belongs to it. A shared
sender, a shared client or a general topic is not enough on its own.

Answer project_index with the number of the chosen candidate. Answer null when
the message does not say which candidate it belongs to, when more than one
candidate fits equally, or when none fits. Null is a normal answer, not a
failure; the application has its own rule for a message you leave unrouted.
Never invent a number that is not in the list.

evidence must be copied VERBATIM from the message's subject or body: the exact
words that show which candidate it is, as a quote of at least two words and at
least 8 characters. The sender line never counts as evidence, because a shared
sender is not enough. Do not paraphrase, summarise or translate it. A choice
whose evidence is not an exact quote of the subject or body, or is shorter than
that, is discarded. When project_index is null, evidence may be empty.

Fields:
  project_index  the number of the chosen candidate from the list, or null.
  evidence       an exact quote of two or more words from the subject or body
                 that decided it.
  reason         one sentence explaining the choice.`

// LaneRoute is the fourth lane (B-D3). Its worker_type is its own because
// every inbox keys its NOT EXISTS on worker_type: a shared value would hide a
// message classified by one lane from another, forever.
var LaneRoute = Lane{
	Name:          "route",
	WorkerType:    "classify_route",
	System:        RouteSystemPrompt,
	PromptVersion: RoutePromptVersion,
	LabelsPath:    "docs/evals/route-from-rules.jsonl",
	Contract:      RouteContract,
}

// RouteCandidate is one row of an account's closed candidate set (B-D1),
// numbered 1-based in the prompt in the order the store returns them.
// Description is source_account_projects.description.
type RouteCandidate struct {
	ProjectID   int64
	Slug        string
	Name        string
	Client      string
	Description string
	IsDefault   bool
}

// ResolveCandidate is the ONE conversion from the model's project_index to a
// candidate, ResolveLink's shape: nil, 0, negative or past the end of the list
// is (zero, false). The result is always one of the account's own rows, never
// a string the model produced.
func ResolveCandidate(cands []RouteCandidate, idx *int) (RouteCandidate, bool) {
	if idx == nil || *idx < 1 || *idx > len(cands) {
		return RouteCandidate{}, false
	}
	return cands[*idx-1], true
}

// The grounding floor (SPEC B-D4 amendment, 2026-09-13), measured on the
// FOLDED evidence: without it any one-letter span ("e", "the", "hi") is a
// substring of nearly every message and grounds a choice for free.
const (
	// GroundMinWords is the fewest whitespace-separated words evidence may have.
	GroundMinWords = 2
	// GroundMinChars is the fewest characters (runes, single spaces included)
	// evidence may have.
	GroundMinChars = 8
)

// Grounded is the gate that replaces a self-reported certainty (B-D4): the
// evidence, whitespace-collapsed and case-folded, is a substring of the subject
// or the body, each field on its own under the same collapse and fold, and is
// at least GroundMinWords words and GroundMinChars characters long. The SENDER
// is deliberately not a field (amended 2026-09-13): a shared sender is not
// enough, so a quote of the sender's own name or address grounds nothing.
// Empty (or whitespace-only) evidence grounds nothing, and a span that
// straddles two fields is a substring of neither.
func Grounded(evidence, subject, body string) bool {
	ev := groundFold(evidence)
	if len(strings.Fields(ev)) < GroundMinWords || utf8.RuneCountInString(ev) < GroundMinChars {
		return false
	}
	for _, field := range []string{subject, body} {
		if strings.Contains(groundFold(field), ev) {
			return true
		}
	}
	return false
}

// groundFold collapses every run of Unicode whitespace (a no-break space
// included) to one ASCII space, trims the ends and lower-cases.
func groundFold(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// routeVerdict mirrors RouteVerdictSchema. ProjectIndex is *int so a JSON null
// and an absent field both decode to nil, which ResolveCandidate rejects.
type routeVerdict struct {
	ProjectIndex *int   `json:"project_index"`
	Evidence     string `json:"evidence"`
	Reason       string `json:"reason"`
}

// routeFields is what a route verdict records in ai_extractions.fields — the
// keys capture's route_apply reads back (project_id, grounded) and the ones
// the report folds (source_account_id, project_slug), plus the bookkeeping every
// lane stores so the report never joins back for a second copy of what was
// classified.
//
//   - project_index is the model's answer VERBATIM (number or null), kept even
//     when rejected, so a pattern of nonsense is visible;
//   - project_id / project_slug are the RESOLVED candidate's, null when the
//     index is null, 0 or out of range;
//   - grounded is true iff a candidate was resolved AND Grounded(evidence, …).
func routeFields(m PendingMessage, v routeVerdict) (map[string]any, bool) {
	var projectID, projectSlug any
	grounded := false
	if c, ok := ResolveCandidate(m.Candidates, v.ProjectIndex); ok {
		projectID, projectSlug = c.ProjectID, c.Slug
		grounded = Grounded(v.Evidence, m.Subject, m.BodyText)
	}
	var index any
	if v.ProjectIndex != nil {
		index = *v.ProjectIndex
	}
	return map[string]any{
		"project_index":         index,
		"project_id":            projectID,
		"project_slug":          projectSlug,
		"grounded":              grounded,
		"evidence":              v.Evidence,
		"reason":                v.Reason,
		"sender":                m.Sender,
		"subject":               m.Subject,
		"channel":               m.Channel,
		"normalized_message_id": m.MessageID,
		"source_account_id":     m.SourceAccountID,
		"candidates":            len(m.Candidates),
	}, grounded
}

// renderRouteUser builds the route prompt's user half: the message (the one
// rendering every lane shares) and then the account's numbered candidates, one
// per line — slug, name, client and the row's description. AFTER the body, so
// a long body truncated by renderMessage can never eat the list. Nothing else:
// no thread context and no links (B-D8).
func renderRouteUser(m PendingMessage) string {
	var b strings.Builder
	b.WriteString(renderMessage(m))
	b.WriteString("\n\nCandidate projects (answer project_index with one number, or null):\n")
	for i, c := range m.Candidates {
		client := c.Client
		if client == "" {
			client = "unknown client"
		}
		fmt.Fprintf(&b, "%d. %s — %s (client: %s): %s\n", i+1, c.Slug, c.Name, client, c.Description)
	}
	return b.String()
}
