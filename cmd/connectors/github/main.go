// github is the interim GitHub PR/CI poller (SPEC 09-jira-github-connectors):
// until hooksd has a public endpoint, it sweeps configured repos with the gh
// token and dispatches the same record_pr_event/record_ci_event intents.
//
//	github --repos owner/repo[,owner/repo...]
//
//	DATABASE_URL  ops db, required
//	GITHUB_TOKEN  falls back to `gh auth token`
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	repos := flag.String("repos", "", "comma-separated owner/repo list (required)")
	flag.Parse()

	if err := run(*repos); err != nil {
		fmt.Fprintln(os.Stderr, "github:", err)
		os.Exit(1)
	}
}

func run(repos string) error {
	if repos == "" {
		return fmt.Errorf("--repos is required")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err != nil {
			return fmt.Errorf("GITHUB_TOKEN not set and `gh auth token` failed: %w", err)
		}
		token = strings.TrimSpace(string(out))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	poller := github.NewPoller(http.DefaultClient, "", token,
		github.NewPGRawStore(pool), github.NewPGTaskResolver(pool, ex, "ghpoll:github"), ex)
	total := 0
	for _, repo := range strings.Split(repos, ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		n, err := poller.PollRepo(ctx, repo)
		total += n
		if err != nil {
			return fmt.Errorf("poll %s: %w", repo, err)
		}
	}
	fmt.Printf(`{"dispatched":%d}`+"\n", total)
	return nil
}
