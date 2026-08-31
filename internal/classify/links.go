package classify

// SWT-25 link-preservation: the application-side half of the index contract.
//
// The model answers with link_index — an integer or null — and THIS is the one
// place an index becomes a URL. The candidate list arrives on
// PendingMessage.Links, scanned from the normalized_messages.links COLUMN; the
// classify Link type is deliberately local rather than imported from the
// connector package, because the contract between the extractor and the
// classifier is the column, not a Go type (the seam has its own integration
// test instead of a shared struct).

// Link is one candidate as stored in normalized_messages.links: {"text","url"},
// ARRAY POSITION is the identity.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// LinkStatus is the outcome of resolving a model's link_index. Four states,
// never collapsed: an alarm that cannot tell "nothing to offer" from "the model
// declined" from "the model answered nonsense" is an alarm nobody can read.
type LinkStatus string

const (
	// LinkNoneOffered: the candidate list was empty. THE COMMON CASE — the two
	// HOA First Notices carry only a tracking pixel — and never an error.
	LinkNoneOffered LinkStatus = "none_offered"
	// LinkNotChosen: candidates were offered and the model answered null (or
	// omitted the field). Ordinary output.
	LinkNotChosen LinkStatus = "not_chosen"
	// LinkResolved: the index named a real position; the Link is one of OUR
	// stored values, never a string the model produced.
	LinkResolved LinkStatus = "resolved"
	// LinkRejected: any index outside 1..len(links) — 0, negative, past the
	// end, or any index at all when the list is empty. Recorded, never an
	// error, never a skip, never fails the message.
	LinkRejected LinkStatus = "rejected"
)

// ResolveLink converts a 1-based link_index into a stored Link.
//
// 1-BASED here and 1-based where the model reads the list, so this is the ONE
// conversion in the codebase: an off-by-one that silently resolves the
// neighbouring URL is worse than a rejected index, because it produces a
// plausible link that is wrong with nothing anywhere to say so.
func ResolveLink(links []Link, idx *int) (Link, LinkStatus) {
	if idx == nil {
		if len(links) == 0 {
			return Link{}, LinkNoneOffered
		}
		return Link{}, LinkNotChosen
	}
	if *idx >= 1 && *idx <= len(links) {
		return links[*idx-1], LinkResolved
	}
	return Link{}, LinkRejected
}
