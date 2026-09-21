package dashboard

import (
	"reflect"
	"testing"
)

// SWT-69 D10 and its review amendment: an EMPTIED box clears the Cc, but an
// input that merely PARSES to zero addresses is not a clear — RFC 5322 group
// syntax does exactly that, with no error — so it is handed to the executor as
// typed and refused by name, never forwarded as [].
func TestSplitCcInput(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"   ", []string{}},
		{"a@x.io", []string{"a@x.io"}},
		{"Billing <b@x.io>, katie@y.org", []string{"b@x.io", "katie@y.org"}},
		{"a@x.io; b@x.io\nc@x.io", []string{"a@x.io", "b@x.io", "c@x.io"}},
		{"undisclosed-recipients:;", []string{"undisclosed-recipients:"}},
		{"not an address", []string{"not an address"}},
	} {
		if got := splitCcInput(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCcInput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
