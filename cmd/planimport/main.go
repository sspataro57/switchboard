// planimport is the one-way plan funnel CLI (SPEC 10-plan-import): propose
// captures a .md plan raw-first and parses it into a proposed task tree
// (dashboard approves at /plans); apply materializes an approved tree through
// the executor and replaces the file with a stub. The import trigger lives in
// a CLI — not the dashboard — because the stub write needs this filesystem.
//
//	planimport propose --project <slug> --file <path>
//	planimport apply --id N
//	planimport list [--status s]
//
//	DATABASE_URL    ops db, required
//	OPENAI_API_KEY  required for propose
//	PLAN_MODEL      default gpt-5-mini
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/planimport"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: planimport <propose|apply|list> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "propose":
		err = proposeCmd(os.Args[2:])
	case "apply":
		err = applyCmd(os.Args[2:])
	case "list":
		err = listCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "planimport:", err)
		os.Exit(1)
	}
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func newExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

func proposeCmd(argv []string) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	project := fs.String("project", "", "project slug (required)")
	file := fs.String("file", "", "plan .md path (required)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *project == "" || *file == "" {
		return fmt.Errorf("usage: planimport propose --project <slug> --file <path>")
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not set")
	}
	model := os.Getenv("PLAN_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}

	content, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	if planimport.IsStub(string(content)) {
		return fmt.Errorf("%s is already an imported-plan stub — new work goes through create_child_task, never plan-file edits", *file)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	client := provider.NewOpenAI(apiKey, os.Getenv("OPENAI_BASE_URL"))
	prop, err := planimport.Propose(ctx, planimport.NewStore(pool), client,
		planimport.Config{Model: model, MaxTokens: 8192}, *project, *file, content)
	if err != nil {
		return err
	}

	ex := newExecutor(pool)
	args, err := json.Marshal(map[string]any{
		"project": prop.ProjectSlug, "source_path": prop.SourcePath,
		"content_hash": prop.ContentHash, "raw_source_item_id": prop.RawSourceItemID,
		"ai_run_id": prop.AIRunID, "ai_extraction_id": prop.AIExtractionID,
	})
	if err != nil {
		return fmt.Errorf("marshal propose args: %w", err)
	}
	res, err := ex.Execute(ctx, executor.Call{Tool: "propose_plan_import",
		Actor: "planimport:" + osUser(), Args: args})
	if err != nil {
		return fmt.Errorf("propose_plan_import: %w", err)
	}
	var out struct {
		PlanImportID int64 `json:"plan_import_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return fmt.Errorf("parse propose result: %w", err)
	}

	fmt.Printf("plan_import %d proposed (project %s)\n", out.PlanImportID, prop.ProjectSlug)
	printTreePreview(ctx, pool, prop.AIExtractionID)
	fmt.Printf("\nreview + approve at /plans/%d, then: planimport apply --id %d\n", out.PlanImportID, out.PlanImportID)
	return nil
}

func printTreePreview(ctx context.Context, pool *pgxpool.Pool, extractionID int64) {
	var fields []byte
	if err := pool.QueryRow(ctx, `SELECT fields FROM ai_extractions WHERE id=$1`, extractionID).Scan(&fields); err != nil {
		return
	}
	var doc planimport.Result
	if json.Unmarshal(fields, &doc) != nil {
		return
	}
	if doc.Summary != "" {
		fmt.Println("summary:", doc.Summary)
	}
	depth := map[string]int{}
	for _, n := range doc.Tasks {
		d := 0
		if n.ParentRef != nil {
			d = depth[*n.ParentRef] + 1
		}
		depth[n.Ref] = d
		deps := ""
		if len(n.DependsOnRefs) > 0 {
			deps = " (after " + strings.Join(n.DependsOnRefs, ", ") + ")"
		}
		fmt.Printf("  %s- [%s] %s%s\n", strings.Repeat("  ", d), n.AssigneeType, n.Title, deps)
	}
	for _, v := range doc.Validation {
		fmt.Println("  note:", v)
	}
}

func applyCmd(argv []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	id := fs.Int64("id", 0, "plan_import id (required)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *id == 0 {
		return fmt.Errorf("usage: planimport apply --id N")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	ex := newExecutor(pool)
	// manual: — the CLI IS Salvador at a keyboard; passes the humanOnly gate.
	res, err := ex.Execute(ctx, executor.Call{Tool: "apply_plan_import",
		Actor: "manual:" + osUser(), Args: []byte(fmt.Sprintf(`{"plan_import_id":%d}`, *id))})
	if err != nil {
		return fmt.Errorf("apply_plan_import: %w", err)
	}
	var out struct {
		Result struct {
			Tasks map[string]int64 `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return fmt.Errorf("parse apply result: %w", err)
	}
	fmt.Printf("applied: %d tasks\n", len(out.Result.Tasks))
	for ref, taskID := range out.Result.Tasks {
		fmt.Printf("  %s -> #%d\n", ref, taskID)
	}

	// Stub replacement — only after a successful apply, only if the file still
	// hashes to the reviewed content (never clobber unreviewed edits).
	var sourcePath, contentHash, slug string
	if err := pool.QueryRow(ctx,
		`SELECT pi.source_path, pi.content_hash, p.slug
		 FROM plan_imports pi JOIN projects p ON p.id = pi.project_id
		 WHERE pi.id=$1`, *id).Scan(&sourcePath, &contentHash, &slug); err != nil {
		return fmt.Errorf("read plan_import for stub: %w", err)
	}
	written, err := planimport.ReplaceWithStub(sourcePath, *id, slug, time.Now().Format("2006-01-02"), contentHash)
	if err != nil {
		return fmt.Errorf("stub write: %w", err)
	}
	if written {
		fmt.Printf("stubbed %s (board is the source of truth now)\n", sourcePath)
	} else {
		fmt.Printf("WARNING: %s missing or edited since propose — stub SKIPPED; reconcile by hand (tasks stand)\n", sourcePath)
	}
	return nil
}

func listCmd(argv []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	status := fs.String("status", "", "filter by status")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	q := `SELECT pi.id, COALESCE(p.slug,''), pi.source_path, pi.status, COALESCE(pi.decided_by,'')
	      FROM plan_imports pi JOIN projects p ON p.id = pi.project_id`
	args := []any{}
	if *status != "" {
		q += ` WHERE pi.status=$1`
		args = append(args, *status)
	}
	q += ` ORDER BY pi.id`
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("select plan_imports: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var slug, path, st, by string
		if err := rows.Scan(&id, &slug, &path, &st, &by); err != nil {
			return fmt.Errorf("scan plan_import: %w", err)
		}
		n++
		fmt.Printf("%-5d %-10s %-24s %-40s %s\n", id, st, slug, path, by)
	}
	if n == 0 {
		fmt.Println("no plan imports (run planimport propose)")
	}
	return rows.Err()
}
