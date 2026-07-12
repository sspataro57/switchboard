package planimport

import "encoding/json"

// PromptVersion tags every ai_runs.input for reproducibility (triage idiom).
const PromptVersion = "plan-import-v1"

// SchemaName is the strict json_schema name sent to the provider.
const SchemaName = "plan_tree"

// SystemPrompt pins the extraction contract: only what the file says, terse
// register, no AI references anywhere (invariant 6 — task bodies leak into
// client-visible drafts downstream).
const SystemPrompt = `You convert a markdown plan file into a task tree for a work board.

Rules:
- Extract ONLY work the file actually describes. Never invent tasks, steps, or
  scope that is not in the file.
- One task per actionable item. Use nesting (parent_ref) for subsections or
  clearly subordinate items. Use depends_on_refs when the file says one item
  comes after another ("after X", "once X is done", ordering words).
- refs are short kebab-case slugs, unique within the plan.
- Titles are short and imperative; bodies carry the file's own wording,
  condensed. Terse, plain register. No pleasantries.
- assignee_type: "claude" for software/automation work a coding agent can do,
  "human" for judgment, communication, or physical-world items.
- priority: 0 by default; 1-3 only when the file marks urgency.
- confidence: how certain you are this item is real discrete work (0-1).
- Never mention AI, assistants, or automation tooling in titles or bodies.
- The tasks array order mirrors the file's order (it becomes the plan order).`

// PlanTreeSchema is the strict JSON Schema forwarded to the provider.
var PlanTreeSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "tasks"],
  "properties": {
    "summary": {"type": "string"},
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["ref", "parent_ref", "title", "body", "assignee_type",
                     "subproject", "worker_type", "priority", "depends_on_refs",
                     "confidence", "notes"],
        "properties": {
          "ref": {"type": "string"},
          "parent_ref": {"type": ["string", "null"]},
          "title": {"type": "string"},
          "body": {"type": "string"},
          "assignee_type": {"type": "string", "enum": ["human", "claude"]},
          "subproject": {"type": ["string", "null"]},
          "worker_type": {"type": ["string", "null"]},
          "priority": {"type": "integer"},
          "depends_on_refs": {"type": "array", "items": {"type": "string"}},
          "confidence": {"type": "number"},
          "notes": {"type": "string"}
        }
      }
    }
  }
}`)
