package slackweb

import (
	"strings"
	"testing"
)

// SWT-39 review: the 0027 preflight's failing case, without a database.
func TestPartialStatusAdmitted(t *testing.T) {
	if err := partialStatusAdmitted("CHECK ((status = ANY (ARRAY['running'::text, 'ok'::text, 'partial'::text, 'error'::text])))"); err != nil {
		t.Errorf("0027's CHECK = %v, want admitted", err)
	}
	err := partialStatusAdmitted("CHECK ((status = ANY (ARRAY['running'::text, 'ok'::text, 'error'::text])))")
	if err == nil || !strings.Contains(err.Error(), "0027") {
		t.Errorf("0001's CHECK = %v, want a refusal naming migration 0027", err)
	}
}
