package provider_test

// SWT-21 criterion 23: NO WORKER MINTS ITS OWN CLIENT.
//
// A plain unit test — no build tag, no database, no network — in the
// internal/textmatch/callsites_test.go shape: it scans sibling packages and
// fails on omission. Criterion 23 covers all THREE worker packages plus capture
// precisely because SWT-18's scan covered three of four matchers and the fourth
// rotted silently for five weeks.
//
// What it is protecting. The boundary is only total if every path to a model
// goes through the Router. A worker that constructs its own provider.OpenAI has
// a lane the Router never sees; a worker whose Run takes a provider.Client has
// no way to be told "not this one, for this message", so the boundary would have
// to live in that worker's own discipline — which is the configuration-shaped
// guard this ticket replaces.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workerPackages are the packages that may USE a model but may never CONSTRUCT
// one. Adapters are built in cmd/ only.
var workerPackages = []string{
	"internal/triage",
	"internal/drafts",
	"internal/planimport",
	"internal/capture",
	// SWT-22 criterion 14: the classifier is a worker like the others. Nothing in
	// internal/classify constructs a client — the Router is built in cmd/classify.
	"internal/classify",
}

// goSources lists the non-test .go files in a package directory, relative to the
// repo root. It fails the test if the directory has none — a scan with nothing
// to scan proves nothing.
func goSources(t *testing.T, pkg string) []string {
	t.Helper()
	dir := filepath.Join("..", "..", pkg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", pkg, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(pkg, n))
	}
	if len(out) == 0 {
		t.Fatalf("no non-test .go files in %s", pkg)
	}
	return out
}

func TestWorkers_DoNotConstructProviderClients(t *testing.T) {
	ctor := regexp.MustCompile(`provider\.New[A-Za-z]*\(`)

	for _, pkg := range workerPackages {
		for _, rel := range goSources(t, pkg) {
			src := repoFile(t, rel)
			for _, m := range ctor.FindAllString(src, -1) {
				t.Errorf("%s calls %s — criterion 23: adapters and routers are constructed in cmd/ ONLY. "+
					"A client minted inside a worker is a lane the locality boundary never sees", rel, m)
			}
		}
	}
}

// The positive control for the scan above. If provider's constructors were
// renamed, or cmd/ stopped building adapters, the ban would still pass — while
// matching nothing anywhere. This asserts the banned token is findable in the
// one place it is allowed, so a green scan means "not there" rather than "not
// anywhere".
func TestProviderConstructors_LiveInCmdOnly(t *testing.T) {
	ctor := regexp.MustCompile(`provider\.New[A-Za-z]*\(`)
	var found []string

	err := filepath.Walk(filepath.Join("..", "..", "cmd"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if ctor.MatchString(string(b)) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	if len(found) == 0 {
		t.Fatalf("no provider.New* call anywhere under cmd/. Either the constructors were renamed (in which " +
			"case TestWorkers_DoNotConstructProviderClients is now scanning for a token that cannot appear) " +
			"or nothing builds an adapter at all")
	}
}

// Criterion 16, 24, 25: the three worker entry points take a *provider.Router,
// not a provider.Client. This is the half of the boundary the COMPILER enforces
// — a fallback would require constructing a second router, which is a visible
// act rather than a forgotten one.
func TestWorkerEntryPoints_TakeARouter(t *testing.T) {
	cases := []struct {
		file string
		fn   string
	}{
		{"internal/triage/triage.go", "func Run("},
		{"internal/drafts/drafts.go", "func Run("},
		{"internal/planimport/planimport.go", "func Propose("},
		// SWT-22 criterion 14. classify.Run takes the ROUTER for the same reason
		// the others do — and here it is what makes "zero hosted calls" a property
		// of the type rather than of the worker's own discipline.
		{"internal/classify/classify.go", "func Run("},
	}

	for _, tc := range cases {
		src := repoFile(t, tc.file)
		i := strings.Index(src, tc.fn)
		if i < 0 {
			t.Errorf("%s does not declare %s", tc.file, tc.fn)
			continue
		}
		// The signature runs to the opening brace of the body.
		sig := src[i:]
		if j := strings.Index(sig, " {"); j > 0 {
			sig = sig[:j]
		}
		if !strings.Contains(sig, "*provider.Router") {
			t.Errorf("%s %s does not take a *provider.Router:\n  %s\nCriteria 16/24/25: a worker holding a "+
				"single Client cannot be told 'not this one, for this message'", tc.file, tc.fn,
				strings.Join(strings.Fields(sig), " "))
		}
		if strings.Contains(sig, "provider.Client") {
			t.Errorf("%s %s still takes a provider.Client:\n  %s", tc.file, tc.fn,
				strings.Join(strings.Fields(sig), " "))
		}
	}
}

// Criterion 25: plan import passes provider.ClassGeneral EXPLICITLY at its single
// call site, with a comment saying why — the input is a plan file a human named
// on the CLI, not captured message content.
//
// Explicit because the zero Class is restricted (criterion 3): without the
// statement, plan import would silently stop working the moment there is no
// local model, and the symptom would be a skip nobody asked for.
func TestPlanImport_DeclaresItsClassExplicitly(t *testing.T) {
	src := repoFile(t, "internal/planimport/planimport.go")
	if !strings.Contains(src, "provider.ClassGeneral") {
		t.Errorf("internal/planimport/planimport.go never names provider.ClassGeneral. Criterion 25 requires " +
			"the class to be passed explicitly at the single call site: relying on the zero value would " +
			"restrict plan import by accident, and the SPEC's reasoning ('a plan file a human named on the " +
			"CLI is not captured message content') would exist nowhere in the code")
	}
}
