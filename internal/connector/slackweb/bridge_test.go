package slackweb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubBridge writes a shell script and points CommandBridge's "node" at /bin/sh,
// so the subprocess contract (argv, stdin, stdout, stderr, exit status) is
// exercised for real without Node or a browser.
func stubBridge(t *testing.T, script string) *CommandBridge {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub bridge: %v", err)
	}
	bridge, err := NewCommandBridge("/bin/sh", path)
	if err != nil {
		t.Fatalf("NewCommandBridge: %v", err)
	}
	return bridge
}

func TestCommandBridgeDraftRejectsSent(t *testing.T) {
	// The Go-side backstop for "the bridge never sends": a leaf that reports a
	// send must fail loudly rather than let the delivery look merely drafted.
	cases := map[string]string{
		"claims sent":     `{"drafted":true,"sent":true}`,
		"not drafted":     `{"drafted":false,"sent":false}`,
		"sent without dr": `{"drafted":false,"sent":true}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			bridge := stubBridge(t, "printf '%s' "+shellQuote(payload))
			err := bridge.Draft(context.Background(), "https://app.slack.com/client/T1/C1", "hi")
			if err == nil {
				t.Fatal("Draft accepted an unsafe result; the never-sends guard is gone")
			}
			if !strings.Contains(err.Error(), "unsafe draft result") {
				t.Fatalf("Draft error = %v, want the unsafe-draft-result guard", err)
			}
		})
	}
}

func TestCommandBridgeDraftAcceptsDraftOnly(t *testing.T) {
	bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"drafted":true,"sent":false}`))
	if err := bridge.Draft(context.Background(), "https://app.slack.com/client/T1/C1", "hi"); err != nil {
		t.Fatalf("Draft(drafted-only) = %v, want nil", err)
	}
}

func TestCommandBridgeDraftRequiresTargetAndText(t *testing.T) {
	bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"drafted":true,"sent":false}`))
	if err := bridge.Draft(context.Background(), "", "hi"); err == nil {
		t.Fatal("Draft accepted an empty target URL")
	}
	if err := bridge.Draft(context.Background(), "https://app.slack.com/client/T1/C1", ""); err == nil {
		t.Fatal("Draft accepted empty text")
	}
}

func TestCommandBridgeDraftPassesRequestOnStdin(t *testing.T) {
	// The operation argument and the JSON request must reach the leaf intact;
	// a silently empty stdin would draft nothing and still report success.
	bridge := stubBridge(t, `
read -r line
case "$1:$line" in
  draft:*app.slack.com/client/T1/C1*hello*) printf '{"drafted":true,"sent":false}' ;;
  *) printf 'bad argv or stdin: %s %s' "$1" "$line" >&2; exit 3 ;;
esac
`)
	if err := bridge.Draft(context.Background(), "https://app.slack.com/client/T1/C1", "hello"); err != nil {
		t.Fatalf("Draft = %v, want the stub to have seen argv 'draft' and the JSON request", err)
	}
}

func TestCommandBridgeExportRejectsSchemaDrift(t *testing.T) {
	// The sibling repo can change independently; a version bump must fail
	// closed rather than normalize a shape we no longer understand.
	bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"schema_version":99,"workspaces":[]}`))
	if _, err := bridge.Export(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("Export error = %v, want an unsupported schema_version refusal", err)
	}
}

func TestCommandBridgeExportParsesWorkspaces(t *testing.T) {
	bridge := stubBridge(t, "printf '%s' "+shellQuote(
		`{"schema_version":1,"workspaces":[{"id":"T1","name":"Avviato","url":"https://app.slack.com/client/T1","own_user_id":"U1","conversations":[]}]}`))
	exported, err := bridge.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(exported.Workspaces) != 1 || exported.Workspaces[0].ID != "T1" ||
		exported.Workspaces[0].OwnUserID != "U1" {
		t.Fatalf("Export workspaces = %+v, want the stub's single T1/U1 workspace", exported.Workspaces)
	}
}

func TestCommandBridgeSurfacesStderrOnFailure(t *testing.T) {
	bridge := stubBridge(t, `printf 'chrome not reachable on 9222' >&2; exit 1`)
	_, err := bridge.Export(context.Background())
	if err == nil || !strings.Contains(err.Error(), "chrome not reachable") {
		t.Fatalf("Export error = %v, want the leaf's stderr surfaced", err)
	}
}

func TestNewCommandBridgeRejectsBadScriptPath(t *testing.T) {
	if _, err := NewCommandBridge("node", ""); err == nil {
		t.Fatal("NewCommandBridge accepted an empty script path")
	}
	if _, err := NewCommandBridge("node", "dist/cli/switchboard-bridge.js"); err == nil {
		t.Fatal("NewCommandBridge accepted a relative script path")
	}
	if _, err := NewCommandBridge("node", filepath.Join(t.TempDir(), "missing.js")); err == nil {
		t.Fatal("NewCommandBridge accepted a nonexistent script")
	}
	if _, err := NewCommandBridge("node", t.TempDir()); err == nil {
		t.Fatal("NewCommandBridge accepted a directory as the bridge script")
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
