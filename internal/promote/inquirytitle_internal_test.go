package promote

import "testing"

// swb 384 / SWT-87: the title names the STORED sender, never the model's asker,
// and never dangles or comes out empty.
func TestInquiryTitle_NamesTheSenderAndNeverDangles(t *testing.T) {
	for _, tc := range []struct{ name, sender, subject, ask, want string }{
		{"task 383: José's mail quoting Katie reads José", `José Garcia <jose.g@avviato.com>`, "Re: partner type",
			"Is there any way that community partner type can not be a location?",
			"José Garcia: Is there any way that community partner type can not be a location?"},
		{"a quoted display name", `"Evans, Katie" <kevans@cecollaboratory.com>`, "", "can you look?", "Evans, Katie: can you look?"},
		{"an address with no display name", "jose.g@avviato.com", "", "ping", "jose.g@avviato.com: ping"},
		{"a Slack display name", "Katie Evans", "", "can you call me", "Katie Evans: can you call me"},
		{"no ask: the subject", "Katie Evans", "WEB-10362 labels", "", "Katie Evans: WEB-10362 labels"},
		{"no ask, no subject: the sender alone", "Katie Evans", "  ", "", "Katie Evans"},
		{"blank sender", "  ", "", "a question", "(unknown sender): a question"},
		{"nothing at all", "", "", "", "(unknown sender)"},
	} {
		if got := inquiryTitle(tc.sender, tc.subject, tc.ask); got != tc.want {
			t.Errorf("%s: inquiryTitle = %q, want %q", tc.name, got, tc.want)
		}
	}
}
