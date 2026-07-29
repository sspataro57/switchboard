package main

// Regression pin for the connector's source selection (SPEC
// imap-mail-connector, acceptance criterion 2; decision 9). ZERO network, ZERO
// Postgres — the selector is a pure function of two env values.
//
// The whole point of MAIL_SOURCE being an EXPLICIT three-way selector is that
// today's behaviour must be byte-identical when it is unset:
//
//   MAIL_SOURCE unset + GMAIL_CONNECTOR_BRIDGE set   => the bridge path
//   MAIL_SOURCE unset + GMAIL_CONNECTOR_BRIDGE unset => the direct Gmail-API path
//
// Inferring the source from source_accounts.auth_type would silently change
// which code path runs the moment a row is added (decision 9); an explicit env
// is greppable in the manifest and testable here.
//
// GREENFIELD NOTE: selectMailSource does not exist yet, so this file
// compile-FAILs — the expected failure mode. Imposed surface (package-internal,
// cmd/connectors/google/main.go):
//
//   type mailSource string
//   const (
//       mailSourceIMAP     mailSource = "imap"
//       mailSourceBridge   mailSource = "bridge"
//       mailSourceGmailAPI mailSource = "gmail_api"
//   )
//   // selectMailSource resolves MAIL_SOURCE + GMAIL_CONNECTOR_BRIDGE into the
//   // ingest path. An unknown MAIL_SOURCE value is an error (never a silent
//   // fallback); "bridge" without a bridge binary is an error.
//   func selectMailSource(mailSourceEnv, bridgeBinary string) (mailSource, error)

import "testing"

func TestSelectMailSource(t *testing.T) {
	cases := []struct {
		name       string
		mailSource string
		bridge     string
		want       mailSource
		wantErr    bool
	}{
		{
			name: "unset + no bridge => today's direct Gmail-API path",
			want: mailSourceGmailAPI,
		},
		{
			name:   "unset + bridge binary => today's bridge path",
			bridge: "/usr/local/bin/gmail-bridge",
			want:   mailSourceBridge,
		},
		{
			name:       "explicit imap",
			mailSource: "imap",
			want:       mailSourceIMAP,
		},
		{
			name:       "explicit imap wins over a configured bridge binary",
			mailSource: "imap",
			bridge:     "/usr/local/bin/gmail-bridge",
			want:       mailSourceIMAP,
		},
		{
			name:       "explicit gmail_api ignores the bridge binary",
			mailSource: "gmail_api",
			bridge:     "/usr/local/bin/gmail-bridge",
			want:       mailSourceGmailAPI,
		},
		{
			name:       "explicit bridge with a binary",
			mailSource: "bridge",
			bridge:     "/usr/local/bin/gmail-bridge",
			want:       mailSourceBridge,
		},
		{
			name:       "explicit bridge without a binary is an error",
			mailSource: "bridge",
			wantErr:    true,
		},
		{
			name:       "an unknown value is an error, never a silent fallback",
			mailSource: "pop3",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectMailSource(tc.mailSource, tc.bridge)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectMailSource(%q, %q) = %q, nil; want an error", tc.mailSource, tc.bridge, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectMailSource(%q, %q): %v", tc.mailSource, tc.bridge, err)
			}
			if got != tc.want {
				t.Errorf("selectMailSource(%q, %q) = %q, want %q", tc.mailSource, tc.bridge, got, tc.want)
			}
		})
	}
}
