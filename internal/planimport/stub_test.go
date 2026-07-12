package planimport_test

// Unit tests for one-way stub replacement (SPEC 10-plan-import, criterion 9).
// After a successful apply the CLI overwrites the plan file with a pinned stub
// pointing at the board — but ONLY if the file still hashes to the content that
// was reviewed; a file edited since propose (hash mismatch) or a missing file
// leaves the tasks standing and SKIPS the overwrite (never clobber unreviewed
// content). `planimport propose` refuses files whose first line carries the
// marker. All offline, temp-dir only — ZERO network, ZERO Postgres.
//
// GREENFIELD NOTE: package internal/planimport does not exist yet; compile-FAIL
// under `go test ./...` is the expected failure mode. Imposed exported surface
// (stub.go), followed by the implementer:
//
//   // The pinned stub marker/body (criterion 9). StubContent renders the full
//   // replacement file; its FIRST line is the machine-readable marker
//   //   <!-- switchboard:imported plan_import={id} project={slug} date={YYYY-MM-DD} -->
//   func StubContent(planImportID int64, slug, date string) string
//
//   // IsStub reports whether content's first line carries the switchboard
//   // import marker (propose refuses such files).
//   func IsStub(content string) bool
//
//   // ContentHash is the PURE plan-file content hash — the value stored in
//   // plan_imports.content_hash and embedded in the raw external_id. It is a
//   // function of the file bytes alone so ReplaceWithStub can recompute it from
//   // the on-disk file.
//   func ContentHash(content []byte) string
//
//   // ReplaceWithStub overwrites path with StubContent IFF the file still hashes
//   // to wantHash. Returns (false, nil) — a SKIP, caller warns — when the file
//   // is missing or its hash differs; never overwrites unreviewed content.
//   func ReplaceWithStub(path string, planImportID int64, slug, date, wantHash string) (written bool, err error)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/planimport"
)

// TestStubContent_CarriesPinnedMarker: the rendered stub's first line is the
// machine marker with id/project/date, and the body carries the board-pointer
// guidance forbidding new work in the file (criterion 9).
func TestStubContent_CarriesPinnedMarker(t *testing.T) {
	body := planimport.StubContent(42, "switchboard", "2026-07-11")

	firstLine := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		firstLine = body[:i]
	}
	for _, want := range []string{
		"switchboard:imported",
		"plan_import=42",
		"project=switchboard",
		"date=2026-07-11",
	} {
		if !strings.Contains(firstLine, want) {
			t.Errorf("stub first line missing %q\nfirst line: %s", want, firstLine)
		}
	}
	// Body guidance (board is source of truth; no new work here).
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "switchboard") || !strings.Contains(lower, "create_child_task") {
		t.Errorf("stub body must point at the board and name create_child_task; got:\n%s", body)
	}
	// A rendered stub is itself detected as a stub.
	if !planimport.IsStub(body) {
		t.Errorf("IsStub(StubContent(...)) = false, want true (round-trip)")
	}
}

// TestIsStub_DetectsMarkerRefusesPlain: the marker on the first line is a stub;
// an ordinary plan file is not.
func TestIsStub_DetectsMarkerRefusesPlain(t *testing.T) {
	if planimport.IsStub("# My plan\n\n- do a thing\n") {
		t.Errorf("IsStub(plain plan) = true, want false")
	}
	stub := planimport.StubContent(7, "acme", "2026-07-11")
	if !planimport.IsStub(stub) {
		t.Errorf("IsStub(stub) = false, want true")
	}
	// Marker must be on the FIRST line — a marker buried mid-file does not count
	// (propose only inspects the first line).
	buried := "# real plan\n<!-- switchboard:imported plan_import=1 project=x date=2026-07-11 -->\n"
	if planimport.IsStub(buried) {
		t.Errorf("IsStub(marker on line 2) = true, want false (only the first line counts)")
	}
}

// TestReplaceWithStub_WritesWhenHashMatches: the happy path — the file still
// hashes to the reviewed content, so it is overwritten with the stub.
func TestReplaceWithStub_WritesWhenHashMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	content := []byte("# Followups\n\n- fix login\n- ship export\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	want := planimport.ContentHash(content)

	written, err := planimport.ReplaceWithStub(path, 99, "switchboard", "2026-07-11", want)
	if err != nil {
		t.Fatalf("ReplaceWithStub: %v", err)
	}
	if !written {
		t.Fatalf("written = false, want true (hash matched)")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after stub: %v", err)
	}
	if !planimport.IsStub(string(got)) {
		t.Errorf("file after ReplaceWithStub is not a stub:\n%s", got)
	}
}

// TestReplaceWithStub_SkipsOnHashMismatch: the file changed since propose — the
// overwrite is SKIPPED and the (unreviewed) content is left untouched.
func TestReplaceWithStub_SkipsOnHashMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	edited := []byte("# Followups (edited after propose)\n\n- new unreviewed item\n")
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	staleHash := planimport.ContentHash([]byte("# Followups\n\n- fix login\n"))

	written, err := planimport.ReplaceWithStub(path, 99, "switchboard", "2026-07-11", staleHash)
	if err != nil {
		t.Fatalf("ReplaceWithStub (mismatch is a skip, not an error): %v", err)
	}
	if written {
		t.Fatalf("written = true, want false (hash mismatch must skip the overwrite)")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after skip: %v", err)
	}
	if string(got) != string(edited) {
		t.Errorf("file was modified on hash mismatch; unreviewed content must be preserved\ngot:\n%s", got)
	}
}

// TestReplaceWithStub_SkipsOnMissingFile: a missing file is a skip, not an error
// (the tasks already stand; there is simply nothing to stub).
func TestReplaceWithStub_SkipsOnMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.md")
	written, err := planimport.ReplaceWithStub(path, 1, "switchboard", "2026-07-11",
		planimport.ContentHash([]byte("anything")))
	if err != nil {
		t.Fatalf("ReplaceWithStub(missing) = %v, want a clean skip", err)
	}
	if written {
		t.Errorf("written = true for a missing file, want false")
	}
}
