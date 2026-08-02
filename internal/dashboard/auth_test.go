package dashboard

import "testing"

// safeNext is the guard on a login redirect, which is the classic place an
// open redirect gets exploited: the victim is mid-authentication and is
// expecting to be sent somewhere, so an off-site hop does not look wrong.
func TestSafeNext(t *testing.T) {
	cases := []struct {
		name string
		next string
		want string
	}{
		{"an in-app path is kept", "/sources", "/sources"},
		{"query string survives", "/tasks?status=ready", "/tasks?status=ready"},
		{"empty falls back", "", defaultLanding},

		// The reason this function exists.
		{"absolute URL is refused", "https://evil.example/steal", defaultLanding},
		{"protocol-relative is refused", "//evil.example/steal", defaultLanding},
		{"scheme-relative with creds is refused", "//user:pw@evil.example", defaultLanding},
		{"bare host is refused", "evil.example", defaultLanding},
		{"backslash is refused", `/\evil.example`, defaultLanding},
		{"CR is refused", "/tasks\r\nSet-Cookie: x=1", defaultLanding},
		{"LF is refused", "/tasks\nX", defaultLanding},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := safeNext(tc.next); got != tc.want {
				t.Errorf("safeNext(%q) = %q, want %q", tc.next, got, tc.want)
			}
		})
	}
}

// The OAuth state parameter carries the requested path across the provider
// round trip. It must survive intact, and a state without one must not break
// the callback — an older in-flight login, or a provider that dropped it.
func TestOAuthStateRoundTrip(t *testing.T) {
	for _, want := range []string{"/sources", "/tasks?status=ready", ""} {
		if got := decodeState(encodeState(want)); got != want {
			t.Errorf("decodeState(encodeState(%q)) = %q", want, got)
		}
	}
	if got := decodeState("sb-state"); got != "" {
		t.Errorf("a state with no path decoded to %q, want empty", got)
	}
	if got := decodeState(""); got != "" {
		t.Errorf("an empty state decoded to %q, want empty", got)
	}
	// The CSRF marker must still lead the state, or the provider round trip
	// stops being checkable at all.
	if s := encodeState("/sources"); s[:len(oauthStateMarker)] != oauthStateMarker {
		t.Errorf("encodeState = %q, want it to start with %q", s, oauthStateMarker)
	}
	// A path containing the separator must not truncate the marker check.
	if got := decodeState(encodeState("/tasks?a=1|2")); got != "/tasks?a=1|2" {
		t.Errorf("a path containing the separator did not survive: %q", got)
	}
}
