package main

// Unit tests for the app-password input path (SPEC imap-mail-connector,
// acceptance criterion 4; decision 14). ZERO network, ZERO Postgres.
//
// The password is read from STDIN ONLY: argv leaks through `ps`, env leaks
// through /proc/*/environ and shell history. stdin also makes the k8s one-liner
// natural:
//
//   kubectl get secret … -o jsonpath=… | base64 -d | google-auth add-app-password …
//
// GREENFIELD NOTE: readAppPassword does not exist yet, so this file
// compile-FAILs — the expected failure mode. Imposed surface (package-internal,
// cmd/google-auth/main.go):
//
//   // readAppPassword consumes the secret from r (stdin in production),
//   // stripping the trailing newline. It errors on an empty input and its
//   // error string NEVER contains the secret.
//   func readAppPassword(r io.Reader) (string, error)

import (
	"strings"
	"testing"
)

func TestReadAppPassword_StripsTheTrailingNewline(t *testing.T) {
	for _, in := range []string{"hunter2secret\n", "hunter2secret\r\n", "hunter2secret"} {
		got, err := readAppPassword(strings.NewReader(in))
		if err != nil {
			t.Fatalf("readAppPassword(%q): %v", in, err)
		}
		if got != "hunter2secret" {
			t.Errorf("readAppPassword(%q) = %q, want %q", in, got, "hunter2secret")
		}
	}
}

// Gmail displays app passwords in four space-separated groups and accepts them
// with or without the spaces; either spelling must survive the read (whether
// the implementation keeps or strips the spaces is its call).
func TestReadAppPassword_AcceptsTheGroupedGmailSpelling(t *testing.T) {
	got, err := readAppPassword(strings.NewReader("abcd efgh ijkl mnop\n"))
	if err != nil {
		t.Fatalf("readAppPassword: %v", err)
	}
	if collapsed := strings.ReplaceAll(got, " ", ""); collapsed != "abcdefghijklmnop" {
		t.Errorf("readAppPassword = %q (collapsed %q), want the 16 characters preserved", got, collapsed)
	}
}

func TestReadAppPassword_RejectsEmptyInput(t *testing.T) {
	for _, in := range []string{"", "\n", "   \n"} {
		if got, err := readAppPassword(strings.NewReader(in)); err == nil {
			t.Errorf("readAppPassword(%q) = %q, nil; want an error (an empty secret must never be stored)", in, got)
		}
	}
}

// Criterion 4: the password appears in no log line and no error string.
func TestReadAppPassword_ErrorNeverEchoesTheInput(t *testing.T) {
	// A too-long line is the only shape that can both fail and carry content.
	secret := strings.Repeat("s3cr3t", 4096)
	got, err := readAppPassword(strings.NewReader(secret + "\n"))
	if err == nil {
		if got != secret {
			t.Errorf("readAppPassword mangled a long secret")
		}
		return
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error string leaks the secret: %v", err)
	}
}
