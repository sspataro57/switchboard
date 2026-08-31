package classify_test

// Unit tests for the application-side half of link preservation (SWT-25,
// acceptance criteria 17, 20, 21 and 22's counters). Fake Store + fake
// provider.Client — ZERO network, ZERO Postgres, ZERO live model.
//
// GREENFIELD NOTE: `internal/classify/links.go` does not exist yet and
// PendingMessage has no Links field, so this file compile-FAILS under
// `go test ./...` — the expected red state. For greenfield code the SPEC's
// contract IS the signature. Imposed surface, from the SPEC's "Internal Go
// surface added" block:
//
//	type Link struct{ Text, URL string }   // scanned from the COLUMN, not imported
//	type LinkStatus string                 // none_offered|not_chosen|resolved|rejected
//	func ResolveLink(links []Link, idx *int) (Link, LinkStatus)
//
//	// PendingMessage gains:
//	//   Links []Link
//	// filled by inboxSelect's COALESCE(nm.links,'[]') — see
//	// links_integration_test.go, which is where the COLUMN half is pinned.
//
// WHY internal/classify DOES NOT IMPORT internal/connector/google: the contract
// between the extractor and the classifier is the COLUMN, not a Go type. Two
// structs agreeing by inspection is how two spellings of one fact drift; the
// seam gets a real integration test (criterion 14) instead of a shared struct.
//
// WHAT THIS FILE CANNOT PROVE, said plainly: every `Links` value below is
// supplied by the fixture, so nothing here shows that the value ever comes out
// of Postgres. That is criterion 16's job and it lives in the integration suite,
// with the mutation named — replace COALESCE(nm.links,'[]') in inboxSelect with
// a literal '[]' and the column test must go red. This repo's sixth landmine is
// exactly a unit test asserting the value it just supplied.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- fakes -------------------------------------------------------------------

// lkClient answers with a caller-chosen verdict per call, which cfClient (one
// canned verdict for the whole pass) cannot do — the four states of criterion 21
// have to coexist inside one pass for the Stats assertions to mean anything.
type lkClient struct {
	desc     provider.Descriptor
	verdicts []string
	calls    int
	requests []provider.Request
}

func (c *lkClient) Describe() provider.Descriptor { return c.desc }
func (c *lkClient) Probe(_ context.Context) error { return nil }

func (c *lkClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	i := c.calls
	c.calls++
	c.requests = append(c.requests, req)
	v := `{"actionable":true,"kind":"payment_due","title":"t","reason":"r","link_index":null}`
	if i < len(c.verdicts) {
		v = c.verdicts[i]
	}
	return provider.Response{Raw: json.RawMessage(v), Model: "qwen3:8b", LatencyMS: 100}, nil
}

func lkLocal(verdicts ...string) *lkClient {
	return &lkClient{
		desc:     provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"},
		verdicts: verdicts,
	}
}

// lkVerdict renders a schema-valid five-field verdict. linkIndex is rendered
// verbatim so a case can pass `null`, `2`, `0` or `99`.
func lkVerdict(linkIndex string) string {
	return fmt.Sprintf(`{"actionable":true,"kind":"payment_due","title":"Pay the HOA fine",`+
		`"reason":"names an amount and a cure-by date","link_index":%s}`, linkIndex)
}

// lkMessage is one inbox row in the shape production yields (inbound, attributed
// to a local_only project), carrying the candidate list the extractor produced.
func lkMessage(id int64, links []classify.Link) classify.PendingMessage {
	return classify.PendingMessage{
		MessageID:        id,
		RawSourceItemID:  1000 + id,
		ThreadID:         id,
		SentAt:           time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Sender:           "notices@pinespropertymanagement.example",
		Subject:          "[#XN123456] Message from Pines Association",
		Channel:          "gmail",
		BodyText:         "A violation was recorded on your property. Cure by 2026-09-15.",
		Direction:        "inbound",
		ProjectID:        7,
		ProjectSlug:      "personal",
		ProjectLocalOnly: true,
		Attribution:      provider.AttrProject,
		Links:            links,
	}
}

func lkTwoLinks() []classify.Link {
	return []classify.Link{
		{Text: "VIEW DETAILS", URL: "https://portal.pinespropertymanagement.com/violations/887"},
		{Text: "PAY NOW", URL: "https://portal.pinespropertymanagement.com/pay"},
	}
}

func lkRun(t *testing.T, store *cfStore, local *lkClient) classify.Stats {
	t.Helper()
	stats, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), local, time.Minute), classify.Config{Model: "qwen3:8b", MaxTokens: 512})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return stats
}

func lkFields(t *testing.T, store *cfStore, i int) map[string]any {
	t.Helper()
	if len(store.extractions) <= i {
		t.Fatalf("wanted extraction %d, got %d extraction(s)", i, len(store.extractions))
	}
	var m map[string]any
	if err := json.Unmarshal(store.extractions[i].fields, &m); err != nil {
		t.Fatalf("ai_extractions.fields is not a JSON object: %v (%s)", err, store.extractions[i].fields)
	}
	return m
}

func lkIntPtr(n int) *int { return &n }

// ---- criterion 20: ResolveLink, and every way an index can be wrong ----------

// The ONE place an index becomes a URL. 1-based where the model reads it and
// 1-based in link_index, so there is exactly one conversion in the codebase —
// `plan_order` is already 1-based sibling position in this repo, and an
// off-by-one that silently resolves the NEIGHBOURING URL is worse than a
// rejected index.
func TestResolveLink_Table(t *testing.T) {
	two := []classify.Link{
		{Text: "VIEW DETAILS", URL: "https://portal.example.com/violations/887"},
		{Text: "PAY NOW", URL: "https://portal.example.com/pay"},
	}

	cases := []struct {
		name       string
		links      []classify.Link
		idx        *int
		wantStatus string
		wantURL    string
		why        string
	}{
		{"no links, no index", nil, nil, "none_offered", "",
			"nothing was offered, so 'the model declined' would be a lie: the two HOA First Notices have no usable link at all"},
		{"no links, empty slice, no index", []classify.Link{}, nil, "none_offered", "",
			"an empty slice and a nil slice are the same fact"},
		{"no links, but an index arrived", nil, lkIntPtr(1), "rejected", "",
			"ANY index at all when the list is empty is out of range — there was no list to index into"},
		{"no links, index 0", nil, lkIntPtr(0), "rejected", "", "same"},
		{"links offered, model answered null", two, nil, "not_chosen", "",
			"an absent field and a JSON null are both 'none of these', which is ORDINARY output"},
		{"first link", two, lkIntPtr(1), "resolved", "https://portal.example.com/violations/887",
			"1-BASED: index 1 is the first entry, the one the numbered list showed as 1."},
		{"last link", two, lkIntPtr(2), "resolved", "https://portal.example.com/pay", ""},
		{"zero is not the first link", two, lkIntPtr(0), "rejected", "",
			"0 is the classic off-by-one. Resolving it to the first entry is the failure that is INVISIBLE: a plausible URL, silently the wrong one"},
		{"negative", two, lkIntPtr(-3), "rejected", "", "no negative indexing, ever"},
		{"len+1", two, lkIntPtr(3), "rejected", "", "one past the end"},
		{"far out of range", two, lkIntPtr(9999), "rejected", "",
			"a model that answered nonsense; recorded as link_index_rejected, never an error"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, status := classify.ResolveLink(tc.links, tc.idx)
			if string(status) != tc.wantStatus {
				t.Errorf("status = %q, want %q. %s", status, tc.wantStatus, tc.why)
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q. A status that is not `resolved` must yield NO url: an "+
					"out-of-range index is never an error, never a skip and never fails the message — but "+
					"it must also never produce a link", got.URL, tc.wantURL)
			}
			if tc.wantURL == "" && got.Text != "" {
				t.Errorf("Text = %q for status %q, want empty — half a resolved link is worse than none",
					got.Text, status)
			}
			if tc.wantStatus == "resolved" && got.Text == "" {
				t.Errorf("a resolved link carries no text; link_text is recorded beside link_url so the " +
					"report can print what the model actually chose")
			}
		})
	}
}

// The four statuses are distinct strings and nothing collapses two of them. An
// alarm that cannot tell "nothing to offer" from "the model declined" from "the
// model answered nonsense" is an alarm nobody can read — the same reason SWT-22
// split its skip reasons.
func TestResolveLink_TheFourStatusesAreDistinct(t *testing.T) {
	two := []classify.Link{{Text: "A", URL: "https://a.example.com/"}, {Text: "B", URL: "https://b.example.com/"}}
	seen := map[string]string{}
	for _, tc := range []struct {
		what   string
		links  []classify.Link
		idx    *int
		expect string
	}{
		{"empty list", nil, nil, "none_offered"},
		{"declined", two, nil, "not_chosen"},
		{"chose", two, lkIntPtr(1), "resolved"},
		{"nonsense", two, lkIntPtr(42), "rejected"},
	} {
		_, st := classify.ResolveLink(tc.links, tc.idx)
		if prev, dup := seen[string(st)]; dup {
			t.Errorf("%q and %q both produce status %q; the four states must never be collapsed",
				prev, tc.what, st)
		}
		seen[string(st)] = tc.what
		if string(st) != tc.expect {
			t.Errorf("%s produced status %q, want %q", tc.what, st, tc.expect)
		}
	}
	if len(seen) != 4 {
		t.Errorf("ResolveLink produced %d distinct statuses, want 4 (none_offered, not_chosen, resolved, "+
			"rejected): %v", len(seen), seen)
	}
}

// ---- criterion 21: four states on ai_extractions.fields, never collapsed ------

func TestClassify_RecordsTheFourLinkStates(t *testing.T) {
	cases := []struct {
		name          string
		links         []classify.Link
		verdict       string
		wantCandidate float64
		wantURL       string
		wantText      string
		wantNullIndex bool
		wantRejected  float64
		hasRejected   bool
		why           string
	}{
		{
			name: "no anchor survived the filter", links: nil, verdict: lkVerdict("null"),
			wantCandidate: 0,
			why: "THE COMMON CASE. The two HOA First Notices have only a tracking pixel; " +
				"link_candidates:0 with no link_url is ordinary output, not a failure",
		},
		{
			name: "candidates offered, model chose none", links: lkTwoLinks(), verdict: lkVerdict("null"),
			wantCandidate: 2, wantNullIndex: true,
			why: "the model looked at two links and said neither is the one to act on — different from " +
				"having nothing to offer, and the counter must be able to say which",
		},
		{
			name: "model chose k", links: lkTwoLinks(), verdict: lkVerdict("1"),
			wantCandidate: 2,
			wantURL:       "https://portal.pinespropertymanagement.com/violations/887",
			wantText:      "VIEW DETAILS",
			why:           "1-based: index 1 is the FIRST entry. link_url is the value the application resolved, never a string the model produced",
		},
		{
			name: "model chose out of range", links: lkTwoLinks(), verdict: lkVerdict("99"),
			wantCandidate: 2, wantRejected: 99, hasRejected: true,
			why: "nonsense is RECORDED, not raised: no skip row, no error, no failed pass — and no link_url",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &cfStore{pending: []classify.PendingMessage{lkMessage(1, tc.links)}}
			local := lkLocal(tc.verdict)
			stats := lkRun(t, store, local)

			if stats.Processed != 1 || stats.Errors != 0 || stats.Skipped != 0 {
				t.Fatalf("stats = %+v, want 1 processed / 0 errors / 0 skipped. %s", stats, tc.why)
			}
			if len(store.withStatus("ok")) != 1 {
				t.Fatalf("recorded %d status='ok' ai_runs rows, want 1", len(store.withStatus("ok")))
			}
			if n := len(store.withStatus("skipped")); n != 0 {
				t.Errorf("recorded %d skipped row(s); nothing about links may skip a message. %s", n, tc.why)
			}
			if len(store.extractions) != 1 {
				t.Fatalf("recorded %d extractions, want 1 — the extraction is what retires the message from "+
					"the inbox, and it is written for EVERY classified message", len(store.extractions))
			}

			f := lkFields(t, store, 0)
			if got, ok := f["link_candidates"].(float64); !ok || got != tc.wantCandidate {
				t.Errorf("fields.link_candidates = %v, want %v. The count is recorded on EVERY verdict: "+
					"without it, 'no candidates' and 'the model declined' are the same row and the report "+
					"cannot tell an operator which. %s", f["link_candidates"], tc.wantCandidate, tc.why)
			}
			if tc.wantURL == "" {
				if _, ok := f["link_url"]; ok {
					t.Errorf("fields carries link_url = %v in the %q state. %s", f["link_url"], tc.name, tc.why)
				}
			} else {
				if got, _ := f["link_url"].(string); got != tc.wantURL {
					t.Errorf("fields.link_url = %v, want %q. The URL comes from ResolveLink against the "+
						"message's own stored candidates — it is a value the APPLICATION resolved from its "+
						"own column, never a string a model produced", f["link_url"], tc.wantURL)
				}
				if got, _ := f["link_text"].(string); got != tc.wantText {
					t.Errorf("fields.link_text = %v, want %q (recorded beside the url so the report can show "+
						"what was chosen)", f["link_text"], tc.wantText)
				}
			}
			if tc.wantNullIndex {
				v, ok := f["link_index"]
				if !ok || v != nil {
					t.Errorf("fields.link_index = %v (present=%v), want an explicit null. 'The model "+
						"declined' is a recorded answer, not an absent one", v, ok)
				}
			}
			if tc.hasRejected {
				if got, ok := f["link_index_rejected"].(float64); !ok || got != tc.wantRejected {
					t.Errorf("fields.link_index_rejected = %v, want %v. An index the model invented is kept "+
						"VERBATIM so a pattern of nonsense is visible in the report; it must not be "+
						"silently dropped into 'the model declined'", f["link_index_rejected"], tc.wantRejected)
				}
			} else if _, ok := f["link_index_rejected"]; ok {
				t.Errorf("fields carries link_index_rejected = %v in the %q state, which did not reject "+
					"anything", f["link_index_rejected"], tc.name)
			}
		})
	}
}

// The zero-candidate message, end to end, called out on its own because it is
// the COMMON case and the SPEC requires it never to look like a failure
// (criterion 21, verification protocol step 7).
func TestClassify_ZeroCandidates_IsOrdinaryOutput(t *testing.T) {
	store := &cfStore{pending: []classify.PendingMessage{lkMessage(1, nil)}}
	local := lkLocal(lkVerdict("null"))

	stats, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), local, time.Minute), classify.Config{Model: "qwen3:8b", MaxTokens: 512})
	if err != nil {
		t.Fatalf("Run returned an error for a message with no link candidates: %v. NO CANDIDATES IS THE "+
			"COMMON CASE — the median personal message has two, and the two HOA First Notices have none at "+
			"all because their only URL is a tracking pixel. A pass that raises on it trains its operator "+
			"to ignore the exit code", err)
	}
	if stats.Processed != 1 || stats.Skipped != 0 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want 1 processed / 0 skipped / 0 errors", stats)
	}
	if len(store.withStatus("ok")) != 1 || len(store.withStatus("skipped")) != 0 {
		t.Errorf("runs = %d ok / %d skipped, want 1 / 0", len(store.withStatus("ok")), len(store.withStatus("skipped")))
	}
	if len(store.extractions) != 1 {
		t.Fatalf("recorded %d extractions, want exactly 1", len(store.extractions))
	}
	if got := lkFields(t, store, 0)["link_candidates"]; got != float64(0) {
		t.Errorf("fields.link_candidates = %v, want 0", got)
	}
}

// ---- criterion 17: the prompt shows TEXTS, numbered, after the body -----------

func TestRenderedPrompt_ListsAnchorTextsByNumberAfterTheBody(t *testing.T) {
	store := &cfStore{pending: []classify.PendingMessage{lkMessage(1, lkTwoLinks())}}
	local := lkLocal(lkVerdict("1"))
	lkRun(t, store, local)

	if len(local.requests) != 1 {
		t.Fatalf("the local client saw %d requests, want 1", len(local.requests))
	}
	user := local.requests[0].User

	for _, want := range []string{"1", "VIEW DETAILS", "2", "PAY NOW"} {
		if !strings.Contains(user, want) {
			t.Errorf("the rendered prompt does not contain %q:\n%s", want, user)
		}
	}
	// Numbered and 1-BASED, in document order: the number the model answers with
	// is the position ResolveLink converts, and there is exactly one conversion.
	one := strings.Index(user, "1. VIEW DETAILS")
	two := strings.Index(user, "2. PAY NOW")
	if one < 0 || two < 0 {
		t.Errorf("the candidate list is not rendered as a 1-based numbered list (`1. VIEW DETAILS`, "+
			"`2. PAY NOW`):\n%s", user)
	} else if one > two {
		t.Errorf("the candidate list is not in document order:\n%s", user)
	}

	// URLs are NOT shown. Honest about what this proves: a text/plain body may
	// legitimately contain URLs of its own, so the prompt is not URL-free in
	// general — this fixture's body has none, deliberately. The STRUCTURAL
	// guarantee that the model cannot answer with a URL is criteria 18-20 (the
	// schema has no url-typed field at all), not this assertion.
	for _, l := range lkTwoLinks() {
		if strings.Contains(user, l.URL) {
			t.Errorf("the rendered prompt contains the URL %q. Criterion 17 renders anchor TEXTS ONLY: the "+
				"model answers with a number, and showing it a URL is showing it the shape of the thing it "+
				"must never author", l.URL)
		}
	}
}

func TestRenderedPrompt_RendersNothingWhenThereAreNoCandidates(t *testing.T) {
	store := &cfStore{pending: []classify.PendingMessage{lkMessage(1, nil)}}
	local := lkLocal(lkVerdict("null"))
	lkRun(t, store, local)

	user := local.requests[0].User
	lower := strings.ToLower(user)
	for _, marker := range []string{"link", "1."} {
		if strings.Contains(lower, marker) {
			t.Errorf("the rendered prompt mentions %q for a message with NO candidates:\n%s\n\n"+
				"Criterion 17: render NOTHING at all when the list is empty. An empty 'Links:' heading is a "+
				"question the model will try to answer, and the whole point of the null case is that it is "+
				"ordinary", marker, user)
		}
	}
}

// The list goes AFTER the body, and the body is truncated at 4000 characters
// (classify.go:405-408) — a list placed before it would be pushed out of a long
// marketing mail, which is exactly the population that has the most anchors.
func TestRenderedPrompt_CandidateListSurvivesALongBody(t *testing.T) {
	m := lkMessage(1, lkTwoLinks())
	m.BodyText = strings.Repeat("marketing filler. ", 500) // > 4000 characters
	store := &cfStore{pending: []classify.PendingMessage{m}}
	local := lkLocal(lkVerdict("null"))
	lkRun(t, store, local)

	user := local.requests[0].User
	if !strings.Contains(user, "VIEW DETAILS") || !strings.Contains(user, "PAY NOW") {
		t.Errorf("the candidate list was lost in a 9000-character body. The body is truncated at 4000 and "+
			"the list is rendered AFTER it, so the truncation can never eat the candidates:\n%s",
			user[max(0, len(user)-400):])
	}
	body := strings.Index(user, "marketing filler")
	list := strings.Index(user, "VIEW DETAILS")
	if body >= 0 && list >= 0 && list < body {
		t.Errorf("the candidate list is rendered BEFORE the body; criterion 17 puts it after")
	}
}

// ---- criterion 22: the counters ----------------------------------------------

// Stats gains Linked and LinkRejected, and they count the two states an operator
// cares about across a pass. Same rule as criterion 21: a counter that cannot
// separate "nothing offered" from "the model answered nonsense" is an alarm
// nobody can read.
func TestStats_CountsLinkedAndRejected(t *testing.T) {
	store := &cfStore{pending: []classify.PendingMessage{
		lkMessage(1, lkTwoLinks()), // resolves
		lkMessage(2, lkTwoLinks()), // rejected
		lkMessage(3, lkTwoLinks()), // declined
		lkMessage(4, nil),          // nothing offered
	}}
	local := lkLocal(lkVerdict("2"), lkVerdict("99"), lkVerdict("null"), lkVerdict("null"))

	stats := lkRun(t, store, local)

	if stats.Processed != 4 || stats.Errors != 0 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want 4 processed / 0 errors / 0 skipped — an out-of-range index must not "+
			"fail, skip or error a message", stats)
	}
	if stats.Linked != 1 {
		t.Errorf("stats.Linked = %d, want 1 (exactly one message resolved to a URL)", stats.Linked)
	}
	if stats.LinkRejected != 1 {
		t.Errorf("stats.LinkRejected = %d, want 1. The rejected count is the one that tells an operator the "+
			"model is answering nonsense — folded into 'not linked' it is invisible, and the null case is "+
			"so common that nobody would notice", stats.LinkRejected)
	}
}
