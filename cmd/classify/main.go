// classify is the LOCAL classifier (SWT-22), SHADOW MODE: it records verdicts
// and creates nothing. Three lanes — personal and residue ask whether mail is
// actionable, inquiry (SWT-33) whether a client message needs a reply.
//
//	classify run     [--lane personal|residue|inquiry] [--limit N] [--since 720h]
//	classify report  [--lane personal|residue|inquiry] [--since 720h]
//	classify eval    [--lane personal|residue|inquiry] [--labels <file>] [--checkpoint <file>]
//	classify promote [--dry-run] [--limit N]
//
// `promote` (SWT-30) is the one deliberate exit from shadow: it turns stored
// PERSONAL-lane verdicts into tasks — whitelisted kinds (payment_due,
// deadline) as live `ready` tasks, everything else parked `holding` — forward
// only from projects.classify_promote_after, deduped per message, everything
// through the executor. It never calls a model (internal/promote cannot even
// import a provider) and takes no cutover flag: the bound is the stored
// column, so eligibility is answerable from the database after the fact.
//
// --lane defaults to personal (SWT-22's lane, unchanged). The residue lane
// (SWT-23) REQUIRES --since on `run`: ~14,737 messages at the measured 7.2 s
// median is ~29.5 GPU-hours, and an unbounded pass must be chosen, not typo'd.
// `eval` defaults --labels to the lane's own fixture, so the residue is never
// scored against the personal labels by omission.
//
// The inquiry lane (SWT-33) asks whether a client message needs a reply from
// Salvador, over projects armed with projects.ai_inquiry. It REQUIRES --since
// on `run` as well — an inquiry goes stale in days. `eval` on ANY lane refuses
// to print a ratio below classify.EvalResultThreshold scored labels.
//
//	DATABASE_URL           ops db, required
//	OPS_LOCAL_PROVIDER_URL local ollama base URL, no /v1
//	OPS_LOCAL_MODEL        required once the URL is set; no fallback
//	CLASSIFY_MODEL         optional override of OPS_LOCAL_MODEL, applied to the
//	                       CLIENT as well as the request so the probe and the
//	                       completion always name the same model
//
// There is NO hosted lane here, and that is the design rather than an omission.
// Every message this worker reads is restricted: the personal lane's through a
// project whose ai_locality is 'local_only', the residue's because an unmatched
// message has no project at all, and the inquiry lane's because classify.Run
// PINS its routed class to restricted — its armed project is ai_locality='any',
// and that nil general client below is what the pin relies on. So a hosted
// client would be refused on every message anyway — building one would only
// create something for a later contributor to "fix" a skip into. The two OPS_LOCAL_* variables the code below reads are documented
// in docs/runbooks/local-classifier.md.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/promote"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

// promoteCmd drives one promotion pass (SWT-30). No --since and no cutover
// flag on purpose: the bound is the stored projects.classify_promote_after.
func promoteCmd(argv []string) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "read the same rows, take the same decisions, write NOTHING; print the plan")
	limit := fs.Int("limit", 0, "max verdicts this pass (0 = all eligible)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// The full executor stack (orchestratord's wiring): validate -> policy ->
	// audit -> handler. Invariant 3 — the promoter reaches tasks, task_events
	// and provenance only through create_task / task_append_log /
	// task_set_source_thread on this executor, as promote:classify.
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	stats, err := promote.Run(ctx, pool, ex, promote.Config{DryRun: *dryRun, Limit: *limit})
	if err != nil {
		return err
	}
	mode := "live"
	if *dryRun {
		mode = "dry-run"
	}
	out, err := json.Marshal(map[string]any{
		"mode": mode, "considered": stats.Considered, "created": stats.Created,
		"review": stats.Review, "attached": stats.Attached, "lost_claims": stats.Lost,
		"reopened": stats.Reopened,
	})
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	fmt.Printf("promote: %s\n", out)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: classify <run|report|eval|promote> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	case "eval":
		err = evalCmd(os.Args[2:])
	case "promote":
		err = promoteCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify:", err)
		os.Exit(1)
	}
}

// buildRouter constructs the ONE lane this worker may use.
//
// general is nil, always. Restricted content is all this worker sees, so a
// hosted client could never serve it — and a nil general lane means there is
// nothing to fall back TO, which is the property the whole boundary exists to
// preserve.
//
// OPS_LOCAL_MODEL is required once the URL is set, with no fallback: guessing a
// model name produces a 404 per message, which reads as a broken adapter and
// trips the unclassified-error raise, instead of an absent lane that skips.
func buildRouter() (*provider.Router, string) {
	base := os.Getenv("OPS_LOCAL_PROVIDER_URL")
	if base == "" {
		slog.Warn("OPS_LOCAL_PROVIDER_URL is not set; every message will be skipped",
			"why", "personal mail is only ever classified locally",
			"fix", "export OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434 (the z4; any RFC1918 IP literal)")
		return provider.NewRouter(nil, nil, 0), ""
	}
	model := os.Getenv("OPS_LOCAL_MODEL")
	if model == "" {
		slog.Error("OPS_LOCAL_PROVIDER_URL is set but OPS_LOCAL_MODEL is empty; the local lane is DISABLED",
			"url", base,
			"why", "guessing a model name would 404 on every message instead of skipping",
			"fix", "export OPS_LOCAL_MODEL=qwen3:8b")
		return provider.NewRouter(nil, nil, 0), ""
	}
	// CLASSIFY_MODEL is applied BEFORE the client is built, and that ordering is
	// the whole point. Applied after, the adapter would probe /api/tags for
	// OPS_LOCAL_MODEL and then POST requests naming a different model: the probe
	// passes, every /api/chat 404s, and a 404 is deliberately not ErrUnavailable
	// — so it lands in the unclassified-error ratio and raises as "a broken
	// adapter, not a busy one" when the adapter is fine and the config is wrong.
	// That is the exact failure the no-fallback rule above exists to avoid, and
	// the first cut of this function had it.
	if m := os.Getenv("CLASSIFY_MODEL"); m != "" {
		slog.Info("CLASSIFY_MODEL overrides OPS_LOCAL_MODEL", "model", m)
		model = m
	}
	local := provider.NewOllama(base, model)
	if loc := provider.LocalityOf(local.Describe()); loc != provider.LocalityLocal {
		slog.Warn("OPS_LOCAL_PROVIDER_URL is not a local endpoint; every message will be skipped",
			"url", base, "locality", loc)
	}
	return provider.NewRouter(nil, local, 0), model
}

func runCmd(argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	laneName := fs.String("lane", classify.LanePersonal.Name, "personal | residue | inquiry")
	limit := fs.Int("limit", 0, "max messages this run (0 = all pending)")
	since := fs.Duration("since", 0, "only messages with sent_at within this window (0 = all; REQUIRED on the residue and inquiry lanes)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	lane, err := classify.LaneByName(*laneName)
	if err != nil {
		return err
	}

	// 6h is DELIBERATE here, unlike evalCmd's old 2h (which killed two full
	// eval runs wearing a provider costume): every run verdict is already an
	// ai_extractions row, so an expired pass resumes naturally on rerun — the
	// worker's extraction dedup skips finished messages. The ceiling bounds a
	// wedged pass, it cannot lose work. A full --since 87600h sweep needs
	// more than one invocation by design.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	st := classify.NewStore(pool)
	ok, release, err := st.TryLock(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("another classify run holds the advisory lock; exiting")
	}
	defer release()

	router, model := buildRouter()
	stats, runErr := classify.Run(ctx, st, router, classify.Config{
		Model: model, MaxTokens: 512, Limit: *limit, Since: *since, Lane: lane,
	})
	out, _ := json.Marshal(stats)
	fmt.Println(string(out))
	return runErr
}

func reportCmd(argv []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	laneName := fs.String("lane", classify.LanePersonal.Name, "personal | residue | inquiry")
	since := fs.Duration("since", 0, "only runs within this window (0 = all)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	lane, err := classify.LaneByName(*laneName)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	return classify.ReportForWorker(ctx, pool, os.Stdout, *since, lane.WorkerType)
}

func evalCmd(argv []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	laneName := fs.String("lane", classify.LanePersonal.Name, "personal | residue | inquiry")
	labelsPath := fs.String("labels", "",
		"the hand-checked labelled set (default: the lane's own fixture)")
	ckptPath := fs.String("checkpoint", "",
		"progress file: verdicts append here and a rerun resumes past them (default: <labels>.progress; removed on success)")
	think := fs.Bool("think", false,
		"A/B EXPERIMENT: enable model thinking (raises MaxTokens to 2048 and num_ctx to 8192; ~4-5x slower). "+
			"Use a DEDICATED --checkpoint — resuming a think run from a non-think progress file mixes verdicts silently")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	lane, err := classify.LaneByName(*laneName)
	if err != nil {
		return err
	}
	if *labelsPath == "" {
		// The lane's own fixture, so the residue is never scored against the
		// personal labels by a forgotten flag.
		*labelsPath = lane.LabelsPath
	}

	labels, err := loadLabels(*labelsPath, lane)
	if err != nil {
		return err
	}
	if *ckptPath == "" {
		*ckptPath = *labelsPath + ".progress"
	}

	// Sized to the BATCH, not to a feeling. The original 2*time.Hour here
	// killed the 874-label residue eval twice, hours in, and both deaths wore a
	// provider costume — the in-flight HTTP call fails with "context deadline
	// exceeded" when the run's own ctx expires, which reads exactly like a
	// stalled ollama. Two minutes per label covers the measured worst case
	// (~40s) five times over; the checkpoint makes even this ceiling cheap.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(len(labels))*2*time.Minute+30*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// buildRouter resolves the model ONCE, into the client itself, so `eval` and
	// `run` cannot score and classify different models — see the CLASSIFY_MODEL
	// note there. Eval prints the model the server reports, which is the truthful
	// answer to "what was this number measured on".
	router, model := buildRouter()
	cfg := classify.Config{Model: model, MaxTokens: 512, Lane: lane, EvalCheckpoint: *ckptPath}
	if *think {
		// The experiment shape, sized by TWO ceilings. 512 reproduces the
		// measured 0.00-score regression and 1536 was exhausted by a real
		// message — thinking length is unbounded. But the budget must also
		// finish inside the adapter's 120s client timeout (~40 gen tok/s at
		// the z4's 90W cap): 4096 did not — the same message then died as a
		// client timeout, which the eval treats as transport and aborts on.
		// 2048 generates in ~70s worst case, safely inside the timeout, so an
		// over-thinker hits num_predict, comes back as provider.ErrIncomplete,
		// and SCORES as a miss — the honest outcome, since >2048 tokens of
		// deliberation per email is not a viable production configuration.
		cfg.Think, cfg.MaxTokens, cfg.NumCtx = true, 2048, 8192
	}
	return classify.Eval(ctx, classify.NewStore(pool), router, cfg, labels, os.Stdout)
}

// subjectHashShape is the only shape classify.SubjectHash produces: 16
// lowercase hex characters.
var subjectHashShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// loadLabels reads the JSONL fixture. It refuses a line carrying message
// CONTENT: the file is committed, and a subject or body in it would put personal
// mail into git — the whole reason the format is ids plus a subject hash.
//
// It also refuses a label outside the LANE's vocabulary (SWT-33 criterion 23):
// its contract's positive token, or "not". Same shape, same keys, same hash —
// the token is the only thing that tells an inquiry file from a personal one,
// and scoring one as the other would print numbers for a different question.
func loadLabels(path string, lane classify.Lane) ([]classify.Label, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open labels: %w", err)
	}
	defer f.Close()

	var out []classify.Label
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		// Repeated keys FIRST: json.Unmarshal keeps only the last value, so a
		// repeat could hide client text behind an allowed value (Codex round 10).
		if k, err := classify.DuplicateLabelKey([]byte(text)); err != nil {
			return nil, fmt.Errorf("%s:%d does not parse as one JSON object: %w", path, line, err)
		} else if k != "" {
			return nil, fmt.Errorf("%s:%d repeats the key %q; JSON keeps only the last value, so an earlier one "+
				"could carry text past every check while it sits in the committed file", path, line, k)
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &probe); err != nil {
			return nil, fmt.Errorf("%s:%d does not parse as JSON: %w", path, line, err)
		}
		for _, banned := range []string{"subject", "body", "body_text", "sender"} {
			if _, bad := probe[banned]; bad {
				return nil, fmt.Errorf("%s:%d carries a %q key; the labelled set is committed and must hold "+
					"NO message content — ids, labels and a subject hash only", path, line, banned)
			}
		}
		// A CLOSED record (SWT-33, Codex round 8): exactly the keys the format
		// defines, so no new field can smuggle content in, and the note is a
		// closed vocabulary rather than free text.
		for k := range probe {
			switch k {
			case "message_id", "label", "subject_sha256", "stratum", "note":
			default:
				return nil, fmt.Errorf("%s:%d carries an unexpected key %q; a label line is message_id, label, "+
					"subject_sha256 and optionally stratum and note — nothing else", path, line, k)
			}
		}
		var l classify.Label
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if !classify.LabelNoteAllowed(l.Note) {
			return nil, fmt.Errorf("%s:%d carries a note outside the closed vocabulary; notes are never free "+
				"text — the file is committed and a free-text note is one paste from a client's message in git",
				path, line)
		}
		// Every other allowed field is constrained too (Codex round 9): the hash
		// is exactly what classify.SubjectHash produces, and the stratum is the
		// closed vocabulary — neither can carry text.
		if !subjectHashShape.MatchString(l.SubjectSHA256) {
			return nil, fmt.Errorf("%s:%d subject_sha256 is not 16 lowercase hex characters (classify.SubjectHash's "+
				"output); the field holds a hash, never text", path, line)
		}
		if !classify.LabelStratumAllowed(l.Stratum) {
			return nil, fmt.Errorf(`%s:%d stratum is outside the closed vocabulary (uniform | enriched | `+
				`domain_gate)`, path, line)
		}
		if l.Stratum != "" && lane.Name == classify.LanePersonal.Name {
			return nil, fmt.Errorf("%s:%d carries a stratum on the personal lane, whose labelled set has none — "+
				"one stray key would switch its eval onto the stratified breakdown", path, line)
		}
		if pos := lane.Contract.PositiveLabel; l.Label != pos && l.Label != "not" {
			return nil, fmt.Errorf(`%s:%d label = %q, want %q or "not" — the %s lane's vocabulary; a file `+
				`labelled for another lane answers a different question`, path, line, l.Label, pos, lane.Name)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read labels: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no labels; there is nothing to score", path)
	}
	return out, nil
}
