// classify is the LOCAL actionability classifier for personal mail (SWT-22),
// SHADOW MODE: it records verdicts and creates nothing.
//
//	classify run    [--limit N] [--since 720h]
//	classify report [--since 720h]
//	classify eval   --labels docs/evals/personal-actionability.jsonl
//
//	DATABASE_URL           ops db, required
//	OPS_LOCAL_PROVIDER_URL local ollama base URL, no /v1
//	OPS_LOCAL_MODEL        required once the URL is set; no fallback
//	CLASSIFY_MODEL         optional override of OPS_LOCAL_MODEL, applied to the
//	                       CLIENT as well as the request so the probe and the
//	                       completion always name the same model
//
// There is NO hosted lane here, and that is the design rather than an omission.
// Every message this worker reads is attributed to a project whose ai_locality
// is 'local_only', so a hosted client would be refused on every message anyway —
// building one would only create something for a later contributor to "fix" a
// skip into. The two OPS_LOCAL_* variables the code below reads are documented
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
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: classify <run|report|eval> [flags]")
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
			"fix", "export OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434")
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
	limit := fs.Int("limit", 0, "max messages this run (0 = all pending)")
	since := fs.Duration("since", 0, "only messages with sent_at within this window (0 = all)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

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
		Model: model, MaxTokens: 512, Limit: *limit, Since: *since,
	})
	out, _ := json.Marshal(stats)
	fmt.Println(string(out))
	return runErr
}

func reportCmd(argv []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	since := fs.Duration("since", 0, "only runs within this window (0 = all)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	return classify.Report(ctx, pool, os.Stdout, *since)
}

func evalCmd(argv []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	labelsPath := fs.String("labels", "docs/evals/personal-actionability.jsonl",
		"the hand-checked labelled set")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	labels, err := loadLabels(*labelsPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
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
	router, _ := buildRouter()
	return classify.Eval(ctx, classify.NewStore(pool), router, labels, os.Stdout)
}

// loadLabels reads the JSONL fixture. It refuses a line carrying message
// CONTENT: the file is committed, and a subject or body in it would put personal
// mail into git — the whole reason the format is ids plus a subject hash.
func loadLabels(path string) ([]classify.Label, error) {
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
		var l classify.Label
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if l.Label != "actionable" && l.Label != "not" {
			return nil, fmt.Errorf(`%s:%d label = %q, want "actionable" or "not"`, path, line, l.Label)
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
