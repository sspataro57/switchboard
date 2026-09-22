package main

// imap-idle-watch (SWT-73) criteria 23 and 24: the runbook section and the kube
// hand-off. ZERO I/O beyond reading two files in this repo.
//
// A test on prose earns its place here the way
// internal/connector/google/runbook_microsoft_test.go and
// internal/mcpserver/runbook_test.go do: nothing in code can stop the next
// session — or Salvador in six months — from reading
// docs/runbooks/imap-mail-connector.md:91-93,136-138, which ALREADY describes
// --watch as if it were resident, and concluding that the deployed watcher
// accepts --full. It does not (D11), and the one-shot flags are exactly what a
// person reaches for when mail looks missing.
//
// Scope, deliberately: the things criteria 23 and 24 enumerate, and nothing
// else. A doc test that pins every sentence makes the doc unmaintainable.
//
// EXPECTED RED: assertion failures — docs/runbooks/HANDOFF-kube-imap-idle-watch.md
// does not exist at all, imap-mail-connector.md has no resident section, and
// calendar-availability.md still says production mail "runs in the watch loop",
// which is false today and is what this ticket makes true.
// (Under a plain `go test ./cmd/connectors/google` the package compile-fails
// first on the greenfield symbols the other new test files name; run this file
// alone with `go test ./cmd/connectors/google/runbook_test.go` to see the
// assertions.)

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const mwRepoRoot = "../../.."

func mwReadDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(mwRepoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// Criterion 23: the runbook's "Running it resident" section.
func TestRunbook_DocumentsTheResidentWatcher(t *testing.T) {
	const rel = "docs/runbooks/imap-mail-connector.md"
	doc := mwReadDoc(t, rel)

	if !regexp.MustCompile(`(?i)^#+\s*running it resident`).MatchString(doc) &&
		!regexp.MustCompile(`(?im)^#+\s*running it resident`).MatchString(doc) {
		t.Errorf("%s has no `Running it resident` section heading (criterion 23). The file already describes "+
			"--watch at :91-93 and :136-138 as if it were deployed; a section is what turns that from a "+
			"half-truth into the operating instructions", rel)
	}

	for _, want := range []struct{ tok, why string }{
		{"MAIL_PASS_TIMEOUT", "D6's bound, with its 10m default — the env table criterion 23 asks for"},
		{"MAIL_WATCH_HEALTH_ADDR", "D7's probe address, default :8092"},
		{"MAIL_RECONCILE_INTERVAL", "the sweep that makes IDLE an optimisation rather than a dependency"},
		{"MAIL_IDLE_REFRESH", "25m, under RFC 2177's 29m ceiling"},
		{"lockkeys.MailWatch", "the singleton key, named so `grep 0x5157` and the runbook agree"},
		{"/healthz", "what the probe means and what it deliberately ignores (D7)"},
		{"imap_idle", "the phase's semantics: an error row per failure, ONE ok row at recovery (D9)"},
		{"--full", "one of the one-shot flags that is NOT available in watch mode (D11)"},
		{"connector-google-watch", "the workload name an operator types into kubectl"},
	} {
		if !strings.Contains(doc, want.tok) {
			t.Errorf("%s never mentions %q — %s (criterion 23)", rel, want.tok, want.why)
		}
	}

	for _, want := range []struct{ re, why string }{
		{`(?i)standby`,
			"a second watcher stands by rather than crashing, and /healthz says so (D4) — otherwise the " +
				"first thing an operator sees during a node drain is a 503 they read as an outage"},
		{`(?i)(ignore|deliberately not|does not).{0,80}idle`,
			"what /healthz deliberately EXCLUDES: IDLE state and mail volume. One mailbox in backoff must " +
				"not restart the pod, and someone WILL propose adding it (D7)"},
		{`(?i)(not available|one-shot only|only.{0,20}one-shot|cannot).{0,120}(--full|watch mode)`,
			"that --full/--overlap/--all/--normalize-only/--calendar-only/--account stay one-shot only " +
				"(D11): a Deployment cannot be asked for a full rescan"},
	} {
		if !regexp.MustCompile(want.re).MatchString(doc) {
			t.Errorf("%s does not say: %s (criterion 23; looked for /%s/)", rel, want.why, want.re)
		}
	}
}

// Criterion 23's second half: the calendar runbook's line 197 is corrected.
// It says today that "production mail runs in the watch loop, which has no
// calendar phase" — which is false (prod mail runs in the CronJob) and, worse,
// is the kind of half-true sentence this ticket turns true in one direction
// while leaving the calendar claim wrong: connector-gcal owns calendar over
// Pipedream, and connector-google's inline calendar phase is a no-op.
func TestRunbook_CalendarOwnershipLineIsCorrected(t *testing.T) {
	const rel = "docs/runbooks/calendar-availability.md"
	doc := mwReadDoc(t, rel)

	if strings.Contains(doc, "production mail\nruns in the watch loop") ||
		strings.Contains(doc, "production mail runs in the watch loop") {
		t.Errorf("%s still carries the uncorrected sentence \"production mail runs in the watch loop, which "+
			"has no calendar phase\". Criterion 23 corrects it to name the workload that OWNS calendar "+
			"(connector-gcal, CAL_SOURCE=pipedream) and to say that connector-google's inline calendar "+
			"phase selects accounts by OAuth credential and is a no-op on the three app-password rows", rel)
	}
	if !strings.Contains(doc, "connector-gcal") {
		t.Errorf("%s never names connector-gcal, the workload that actually owns the calendar phase "+
			"(criterion 23)", rel)
	}
}

// Criterion 24: the kube hand-off. The manifests belong to the kube session
// (IK: "kube manifests belong to the kube session"), so this file is the entire
// interface between the two sessions — and D3's env-parity table is a GATE, not
// advice: drift in CAPTURE_RULES_MODE, CAPTURE_RULES_SINCE or
// MAIL_MAX_MESSAGE_BYTES gives two behaviours on one mailbox depending on which
// process won the per-account lock, which is invisible in logs.
func TestHandoff_CarriesEverythingTheKubeSessionNeeds(t *testing.T) {
	const rel = "docs/runbooks/HANDOFF-kube-imap-idle-watch.md"
	doc := mwReadDoc(t, rel)

	for _, want := range []struct{ tok, why string }{
		{"connector-google-watch", "the Deployment's name (D2)"},
		{"--watch", "the argument that selects the resident mode"},
		{"Recreate", "the update strategy: a rolling update would run two watchers, both resolving the MSN " +
			"credential (D2, D5)"},
		{"terminationGracePeriodSeconds", "120s, so an in-flight pass finishes or is cancelled cleanly (D2)"},
		{"livenessProbe", "the probe on /healthz (D7)"},
		{"/healthz", "what the kubelet asks"},
		{"8092", "the port the probe targets, which must equal the code's default"},
		{"MS_OAUTH_CLIENT_ID", "the variable the 2026-09-18 correction says a watch workload also needs"},
		{"CAPTURE_RULES_MODE", "the parity variable whose drift makes the watcher a silent no-op"},
		{"CAPTURE_RULES_SINCE", "the parity variable with the 2h floor landmine"},
		{"MAIL_MAX_MESSAGE_BYTES", "the third parity variable D3 names"},
		{"MQTT_BROKER", "unset, AnnounceCaptured skips and instant mail does not become instant tasks"},
		{"OPS_TOKEN_KEY", "without it the watcher cannot decrypt a single credential"},
		{"kubectl", "the exact command that prints both env lists for the parity check"},
		{"0 */2 * * *", "the CronJob's new schedule under OQ-1 = B-reduced"},
		{"*/10 * * * *", "the schedule it is moved FROM, and the rollback value"},
		{"suspend", "the OQ-1 answer's consequence: connector-google is NOT suspended"},
	} {
		if !strings.Contains(doc, want.tok) {
			t.Errorf("%s never mentions %q — %s (criterion 24)", rel, want.tok, want.why)
		}
	}

	for _, want := range []struct{ re, why string }{
		{`(?i)digest|sha256:`, "the image digest, beside the tag: a tag is mutable and a hand-off that names " +
			"only a tag cannot be audited later"},
		{`(?i)roll ?back|rollback`, "the rollback: scale the Deployment to 0 and put the CronJob back on */10"},
		{`(?i)(deployment first|order matters|only after|then).{0,200}(cronjob|schedule)`,
			"the ORDER: the Deployment first, verified with a 200 on /healthz and one observed wake, and " +
				"only THEN the CronJob's schedule change. Never the reverse — a gap with neither running is " +
				"a mail outage nobody would notice for ten minutes"},
	} {
		if !regexp.MustCompile(want.re).MatchString(doc) {
			t.Errorf("%s does not carry: %s (criterion 24; looked for /%s/)", rel, want.why, want.re)
		}
	}
}
