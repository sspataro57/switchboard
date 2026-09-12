package mcpserver_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criteria 21 and 22: the user-scope
// install runbook. A test on prose earns its place the way
// internal/ticketstatus's TestRunbook_DocumentsTheReconciler does — nothing in
// code can stop the next session installing ops-mcp with `go run` from a ticket
// branch, or installing the full ops-mcp instead of the user binary so that
// every repo's session gets the whole write surface.
//
// AMENDED after review (2026-09-10): the runbook first claimed that omitting
// OPS_TOKEN_KEY from the -e flags kept sends out of other repos. It does not — a
// stdio server inherits the launching shell's environment, and ~/.bashrc exports
// the key. The boundary is the separate binary (a profile SETTING on ops-mcp
// would have had to default to full), and the runbook must say both.
//
// AMENDED for SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criteria 24 and 25:
// the binary is renamed cmd/ops-mcp-read → cmd/ops-mcp-user (V4) and serves the
// user profile, six tools including task_dismiss, task_close and
// task_mark_delivered (V3). The runbook keeps its path and must now carry the
// migration from the old registration, the accepted risk (V0) with its recovery
// (task_reopen), the swb verb lines, the dismissal provenance note (V7), the
// full profile's new count (22), and no `claude mcp add` line naming
// ops-mcp-read. EXPECTED RED until docs/runbooks/ops-mcp-user-scope.md is
// rewritten.
//
// AMENDED for SWT-38 (docs/tickets/mcp-task-capture_SPEC.md) criteria 22 and
// 23: the user profile gains create_task, task_append_log and
// task_set_priority (nine tools), and the full profile goes 22 → 23. The
// runbook must name the three tools and the top level (urgent), extend the
// accepted risk to creating tasks and reordering priority (C9) with its
// recovery, say that no worker console picks up what a session creates (C1),
// carry the new usage lines and an "Upgrading from SWT-37" block, and no longer
// claim the install "cannot create, claim, … log". The one existing
// requirement whose VALUE changes is the full-profile count (`22 tools` →
// `23 tools`, criterion 22); every other SWT-35/37 requirement is unchanged.
//
// AMENDED — not loosened — for SWT-42 (docs/tickets/mail-attachments_SPEC.md)
// criterion 24: the user profile gains mail_list_attachments and
// mail_read_attachment (eleven tools; owner decision O1) and the full profile
// goes 23 → 25. Two requirements change VALUE: `\bnine tools\b|\b9 tools\b`
// becomes `\beleven tools\b|\b11 tools\b`, and `\b23 tools\b` becomes
// `\b25 tools\b` — and the stale counts are now refused outright, since a
// runbook saying "nine tools" after this ticket is a false claim about the
// boundary. New: the two tool tokens, the title's `swt-42`, the usage line
// (find Sana's attachment → mail_list_attachments by sender/subject →
// mail_read_attachment), "reads attachments of non-private mail only" and
// "read mail bodies" in the can/cannot paragraphs, and the attachment
// accepted-risk paragraph naming untrusted content and the task verbs it could
// trigger. Every other SWT-35/37/38 requirement is unchanged. EXPECTED RED
// until the runbook is rewritten.
//
// AMENDED — not loosened — for SWT-44 (user-profile-drafts): the user profile
// gains draft_delivery and update_delivery (thirteen tools) and the full
// profile goes 25 → 26 (update_delivery is MCP-listed). The stale list gains
// eleven/11/25. The review fixes add: gmail drafting in the intro's "and
// nothing else", gmail only, own drafts only, the dashboard showing From and To
// before approval, a planted-draft recovery (until SWT-43's Deny), drafting in
// the accepted risk, both tools in the verify step's audit query, and three
// sentences that are now false are refused outright.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunbook_DocumentsUserScopeInstall(t *testing.T) {
	const rel = "docs/runbooks/ops-mcp-user-scope.md"
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v. Criterion 21: the one-time user-scope install is the ticket's 'usable "+
			"alone' step, and its runbook is new", rel, err)
	}
	doc := string(b)
	lower := strings.ToLower(doc)

	// ---- the tokens, exactly as spelled ------------------------------------
	for _, want := range []struct{ tok, why string }{
		{"--scope user", "the install is USER scope, so every repo's session sees the `ops` server"},
		{"OPS_WORKER_ID=manual:salvo", "REQUIRED: ops-mcp refuses to start without it (fact 4), and a manual install is manual:salvo"},
		{"go install ./cmd/ops-mcp-user", "a BUILT binary from main, never `go run` of whatever branch is checked out (L13); SWT-37 renamed it"},
		{"OPS_TOKEN_KEY", "named, so the reader learns that omitting it is NOT the boundary (L13)"},
		{"ops-mcp-user", "the boundary: the user binary lists its tools and wires no sender (SWT-37 V4)"},
		// SWT-37 criterion 24: the six tools.
		{"project_list", "the directory a session confirms a slug against"},
		{"task_list", "the queue read itself"},
		{"task_get_next", "the third queue read"},
		{"task_dismiss", "SWT-37: a user-profile verb"},
		{"task_close", "SWT-37: a user-profile verb"},
		{"task_mark_delivered", "SWT-37: a user-profile verb"},
		{"delivered", "the default hides delivered work, unlike the board (L4)"},
		// SWT-37 criterion 25.
		{"claude mcp remove", "the migration removes the old registration before re-adding (V4)"},
		{"task_reopen", "the recovery for a wrong close or dismiss (V0)"},
		// SWT-38 criterion 23: the three capture tools and the top level.
		{"create_task", "SWT-38: the user profile creates HUMAN tasks (C1)"},
		{"task_append_log", "SWT-38: progress lines, on human tasks only (C4)"},
		{"task_set_priority", "SWT-38: reorder any task's priority (C5)"},
		{"urgent", "SWT-38: the level table (normal 0, elevated 1, high 2, urgent 3)"},
		// SWT-42 criterion 24: the two attachment tools.
		{"mail_list_attachments", "SWT-42: list a message's attachments (by id, or the sender/subject finder)"},
		{"mail_read_attachment", "SWT-42: read one attachment inline, or to a file"},
	} {
		if !strings.Contains(doc, want.tok) {
			t.Errorf("%s never mentions %q — %s", rel, want.tok, want.why)
		}
	}

	// ---- literal lines ----------------------------------------------------
	for _, want := range []struct{ re, why string }{
		{`claude mcp add --scope user ops`, "the `claude mcp add` line, server name `ops` (L13: the name is what makes precedence work)"},
		{`-e DATABASE_URL=`, "the first of the two -e flags"},
		{`-e OPS_WORKER_ID=manual:salvo`, "the second -e flag"},
		{`-- "$(go env GOPATH)/bin/ops-mcp-user"`, "the registered command is the USER binary (SWT-37 criterion 25)"},
		{`claude mcp remove --scope user ops`, "SWT-37 V4 migration step 2"},
		{`mcp:manual:salvo`, "verification: the audit rows carry the manual actor"},
		{`claude mcp get ops`, "verification step 4"},
		{`/mcp`, "verification: `/mcp` shows `ops` connected from another repo, and ONE `ops` in this one"},
	} {
		if !strings.Contains(doc, want.re) {
			t.Errorf("%s does not contain %q — %s", rel, want.re, want.why)
		}
	}

	// ---- prose, case-insensitive --------------------------------------------
	for _, want := range []struct{ re, why string }{
		{`\.mcp\.json`, "the precedence note names the project-scope entry that shadows the user one in this repo"},
		{`shadow|precedence|local\s*(→|->)\s*project\s*(→|->)\s*user`,
			"the precedence note: Claude Code resolves a same-name server local → project → user (L13)"},
		{`(?s)ops_token_key.{0,300}not a boundary.{0,400}inherit`,
			"WHY omitting OPS_TOKEN_KEY protects nothing: the server inherits the launching shell's environment"},
		{`(?s)ops-mcp-user. is the boundary.{0,600}wires no mail sender`,
			"WHAT the user binary does: whatever the environment holds, no sender is wired (SWT-37 criterion 25)"},
		{`never install .ops-mcp. itself at user scope`, "the full binary is named as the thing NOT to install (kept verbatim)"},
		{`re-?run|re-?install`, "the re-install rule: re-run `go install` after any merge touching the server"},
		{`cmd/ops-mcp-user`, "…naming what triggers it: cmd/ops-mcp-user"},
		{`internal/mcpserver`, "…internal/mcpserver"},
		{`internal/tools`, "…internal/tools"},
		{`internal/policy`, "…and internal/policy, which now holds the gate on the three verbs (SWT-37 criterion 24)"},
		{`remember|memori[sz]e`, "the memorise-a-slug usage line: the per-repo binding lives in Claude Code's memory (L2)"},
		{`(?s)closed.{0,300}delivered|delivered.{0,300}closed`,
			"one sentence: task_list hides closed AND delivered by default…"},
		{`board`, "…unlike the board, which hides only closed"},
		// SWT-37 criterion 24/25.
		{`new session`, "tools and Instructions are fetched at initialize: open a NEW session after re-registering"},
		{`(?s)(email|web page).{0,300}(dismiss|close).{0,400}reopen`,
			"the ACCEPTED RISK (V0): untrusted text read in any repo can dismiss or close a task; recovery is a reopen"},
		{`(?s)go install \./cmd/ops-mcp-user.{0,600}claude mcp remove --scope user ops.{0,600}claude mcp add --scope user ops.{0,600}rm -f .{0,80}ops-mcp-read`,
			"the V4 migration in its fail-safe ORDER: install → remove → add → delete the old binary last"},
		{`swb dismiss`, "the usage line for task_dismiss"},
		{`swb close`, "the usage line for task_close"},
		{`swb delivered`, "the usage line for task_mark_delivered"},
		{`dismissed_by`, "the dismissal provenance note (V7): mcp:… labels were mapped by a model, dashboard:… picked by Salvador"},
		// Was `\b22 tools\b` (SWT-37 criterion 24, 19 → 22), then `\b23 tools\b`
		// (SWT-38 criterion 22). SWT-42 criterion 24 moves the full profile to 25
		// with the two attachment tools.
		// SWT-44: update_delivery joins the full profile (25 → 26).
		{`\b26 tools\b`, "the full profile's tool count, 25 → 26 (SWT-44)"},
		// SWT-38 criterion 23.
		{`(?s)(email|web page).{0,600}(creat|priorit).{0,600}task_set_priority`,
			"the ACCEPTED RISK extended (C9): untrusted text can also create tasks and reorder priority; one task_set_priority puts it back"},
		{`(?i)no worker console`, "C1: what a session creates is human work, which no worker console picks up"},
		// SWT-38 criterion 22: the usage lines and the upgrade block.
		{`swt-38`, "the title keeps SWT-38"},
		{`swb add`, "usage: 'swb add <title>' → create_task"},
		{`swb log`, "usage: 'swb log <id> <text>' → task_append_log"},
		{`swb done`, "usage: 'swb done <id>' → task_close with the outcome"},
		{`swb prioritize`, "usage: 'swb prioritize <id> [level]' → task_set_priority"},
		{`deprioritize`, "usage: 'swb deprioritize <id>' → normal"},
		{`(?s)upgrading from swt-37.{0,600}go install \./cmd/ops-mcp-user.{0,600}new session`,
			"the upgrade block: go install on main, then a NEW session — the registration is unchanged"},
		// Was `\bnine tools\b|\b9 tools\b` (SWT-38 criterion 22), then
		// `\beleven tools\b|\b11 tools\b` (SWT-42 criterion 24). SWT-44:
		// draft_delivery and update_delivery make it thirteen.
		{`\bthirteen tools\b|\b13 tools\b`, "the user profile's tool list becomes thirteen (SWT-44)"},
		{`swt-44`, "the title gains SWT-44"},
		{`(?s)draft_delivery.{0,400}update_delivery.{0,600}(approve|send).{0,200}dashboard`,
			"SWT-44: the session drafts and fixes a reply; approving and sending stay on the dashboard"},
		// SWT-44 review fixes: what the drafting actually is, and how to undo it.
		{`(?s)draft gmail repl.{0,200}and nothing else`, "the intro's 'and nothing else' includes gmail drafting"},
		{`(?s)draft_delivery.{0,300}gmail only`, "fix 3: the user profile drafts gmail replies only"},
		{`its own drafts`, "fix 2: update_delivery edits only the session's own drafts"},
		{`(?s)dashboard shows.{0,80}from.{0,40}to.{0,80}before`, "fix 4: the dashboard shows From and To before approval"},
		{`(?s)planted draft.{0,400}(dashboard|unapproved)`, "Recovery: a planted draft is edited on the dashboard or left unapproved"},
		{`(?s)swt-43.{0,200}deny`, "…until SWT-43's Deny ships"},
		{`(?s)untrusted.{0,600}draft`, "Accepted risk: drafting from sessions that read untrusted content"},
		{`tool in \([^)]*'draft_delivery'[^)]*'update_delivery'`, "the verify step's audit query lists both new tools"},
		// SWT-42 criterion 24.
		{`swt-42`, "the title gains SWT-42"},
		{`(?s)find the attachment sana sent.{0,400}mail_list_attachments.{0,300}(sender|subject).{0,400}mail_read_attachment`,
			"usage: 'find the attachment Sana sent about the Activities Integration' → mail_list_attachments by sender/subject, then mail_read_attachment"},
		{`reads attachments of non-private mail only`, "the can-do paragraph gains the attachment reads, and says which mail"},
		{`read mail bodies`, "'What it cannot do' still says it cannot read mail bodies: mail_search / mail_read_thread stay full-profile only"},
		{`(?s)attachment.{0,400}(untrusted|stranger|someone else)|(untrusted|stranger|someone else).{0,400}attachment`,
			"the attachment ACCEPTED RISK names untrusted content: a tool whose whole job is fetching outside text"},
		{`(?s)attachment.{0,400}(dismiss|close|mark(ed)? delivered).{0,600}creat.{0,600}(reorder|priorit)`,
			"…and the task verbs such text could trigger: dismiss/close/mark delivered, create, reorder"},
	} {
		if !regexp.MustCompile(want.re).MatchString(lower) {
			t.Errorf("%s does not match /%s/ — %s", rel, want.re, want.why)
		}
	}

	// SWT-42 criterion 24: the superseded counts are false claims now.
	for _, stale := range []string{`\bnine tools\b`, `\b9 tools\b`, `\b23 tools\b`, `\beleven tools\b`, `\b11 tools\b`, `\b25 tools\b`} {
		if regexp.MustCompile(stale).MatchString(lower) {
			t.Errorf("%s still matches /%s/: since SWT-44 the user profile lists thirteen tools and the full "+
				"profile 26", rel, stale)
		}
	}

	// SWT-44 review: sentences that stopped being true when the user profile
	// started drafting. The session picks the thread, so the recipient is not
	// "never from the session"; and a session now creates and edits deliveries.
	for _, lie := range []string{"never from the session", "no delivery is created or changed", "no delivery is touched"} {
		if strings.Contains(lower, lie) {
			t.Errorf("%s still says %q: since SWT-44 a session drafts and edits delivery rows (never approves or "+
				"sends), and it chooses the thread the recipient comes from", rel, lie)
		}
	}

	// SWT-38 criterion 22: the boundary sentence "It cannot create, claim, …
	// log, …" is REWRITTEN — since SWT-38 the install creates human tasks and
	// logs on them, so the old sentence would be a false boundary claim.
	if strings.Contains(lower, "cannot create, claim") {
		t.Errorf("%s still says the install \"cannot create, claim, …\": SWT-38 lets it create HUMAN tasks, log on "+
			"human tasks and set priority; the boundary paragraph must say what it can and cannot do now", rel)
	}

	// SWT-37 criterion 25: the OLD binary may appear only in the migration's
	// clean-up, never as what a `claude mcp add` registers.
	adds := 0
	for _, line := range strings.Split(doc, "\n") {
		if !strings.Contains(line, "claude mcp add") {
			continue
		}
		adds++
		if strings.Contains(line, "ops-mcp-read") {
			t.Errorf("%s has a `claude mcp add` line naming ops-mcp-read: %q — the old read-only binary is gone "+
				"(SWT-37 V4)", rel, line)
		}
	}
	if adds == 0 {
		t.Errorf("POSITIVE CONTROL: %s has no `claude mcp add` line at all", rel)
	}
}
