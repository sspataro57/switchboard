package tools

// Test-only export for SWT-35 criterion 14 (docs/tickets/task-list-mcp_SPEC.md,
// L5). tasklist_integration_test.go lives in package tools_test beside the
// shared integration helpers (newToolsPool, cleanupToolsData, …) and must
// compare the validator's status list with the live tasks.status CHECK read via
// pg_get_constraintdef. This file is compiled only into the package's own test
// binary; it adds nothing to the production package.
//
// GREENFIELD NOTE — EXPECTED RED until internal/tools/tasklist.go declares
// `var taskStatuses []string`.

// TaskStatusesForTest is taskStatuses, the list validateTaskList accepts.
var TaskStatusesForTest = taskStatuses
