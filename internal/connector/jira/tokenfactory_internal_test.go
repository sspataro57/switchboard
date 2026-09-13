package jira

// SWT-40 Part D review fix 3: the token-built client (the gate's, the
// reconciler's and the poller's) has a request timeout. It used
// http.DefaultClient, which has none: one hung Jira socket would hold a pass
// until its context deadline.

import (
	"net/http"
	"testing"
	"time"
)

func TestTokenClient_HasARequestTimeout(t *testing.T) {
	if LookupRequestTimeout != 30*time.Second {
		t.Errorf("LookupRequestTimeout = %v, want 30s", LookupRequestTimeout)
	}
	c := newTokenClient(Account{ID: 1, Email: "itest@example.test", SiteBaseURL: "http://jira.example.test"}, "tok")
	if c.hc == nil || c.hc == http.DefaultClient {
		t.Fatalf("the token client uses http.DefaultClient (no timeout)")
	}
	if c.hc.Timeout != LookupRequestTimeout {
		t.Errorf("token client Timeout = %v, want %v", c.hc.Timeout, LookupRequestTimeout)
	}
	if c.baseURL != "http://jira.example.test" || c.email != "itest@example.test" || c.token != "tok" {
		t.Errorf("token client = %+v, want the account's site, email and the decrypted token", c)
	}
}
