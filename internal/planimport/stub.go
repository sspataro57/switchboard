package planimport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// stubMarker is the machine-readable prefix of a stub's first line.
const stubMarker = "<!-- switchboard:imported"

// ContentHash is the PURE plan-file content hash — the value stored in
// plan_imports.content_hash and embedded in the raw external_id. A function of
// the file bytes alone so ReplaceWithStub can recompute it from disk.
func ContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// StubContent renders the full replacement file (criterion 9). Its FIRST line
// is the machine marker propose refuses.
func StubContent(planImportID int64, slug, date string) string {
	return fmt.Sprintf(`<!-- switchboard:imported plan_import=%d project=%s date=%s -->
# Imported into switchboard

This plan is tracked on the switchboard board (project %q) — the
board is the source of truth now. Do NOT add work here: new discovered
work = create_child_task.
`, planImportID, slug, date, slug)
}

// IsStub reports whether content's FIRST line carries the import marker.
func IsStub(content string) bool {
	first := content
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		first = content[:i]
	}
	return strings.HasPrefix(strings.TrimSpace(first), stubMarker)
}

// ReplaceWithStub overwrites path with StubContent IFF the file still hashes
// to wantHash. Returns (false, nil) — a SKIP, caller warns — when the file is
// missing or its hash differs: the tasks already stand, but content that was
// never reviewed must not be clobbered.
func ReplaceWithStub(path string, planImportID int64, slug, date, wantHash string) (bool, error) {
	current, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if ContentHash(current) != wantHash {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(StubContent(planImportID, slug, date)), 0o644); err != nil {
		return false, fmt.Errorf("write stub %s: %w", path, err)
	}
	return true, nil
}
