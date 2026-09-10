package classify

// SWT-23's lane split, as SWT-33 (2026-09-10) amended it: there are EXACTLY
// THREE lanes and EXACTLY TWO contracts. A lane still costs a worker_type, a
// prompt and a population whose class somebody has to argue about, and that
// argument still belongs in a SPEC rather than in this var block — the
// structural test that counts these is where it gets written down.
//
// What a Lane is NOT: a schema, casually. LanePersonal and LaneResidue share
// ONE Contract on purpose — that is the only thing making the residue's
// recall/precision readable against the personal lane's 0.94 / 0.50, and a
// per-lane schema would have made those two lanes two experiments.
// LaneInquiry carries a SECOND contract because it asks a genuinely different
// question ("does this need a reply from me", not "is this actionable"), so
// scoring one against the other's labels would compare nothing to nothing.
// Two contracts, three lanes, and the assertion that keeps the first sentence
// true is LanePersonal.Contract == LaneResidue.Contract.
//
// The residue lane's containment story, stated here because criterion 15 says
// the honesty label is re-stated rather than inherited: a residue row has NO
// project (0015's CHECK — an unmatched decision has a NULL project_id), so
// ProjectLocalOnly is false and the ai_locality column cannot restrict it. What
// restricts it is Attribution = AttrUnmatched: ClassOf maps every state that is
// not AttrProject to ClassRestricted (provider/locality.go), which is SWT-21's
// deliberate choice so that rule completeness is never load-bearing for
// containment. Two lanes, two reasons, one outcome — and neither reason is the
// class fold acting as a guard.

import (
	"encoding/json"
	"fmt"
)

// Contract is the output shape a lane's verdicts take: the structured-output
// schema the provider is given, and the field names everything downstream reads
// them back by. Shared by the two actionability lanes; distinct for inquiry.
type Contract struct {
	SchemaName    string          // provider.Request.SchemaName
	Schema        json.RawMessage // provider.Request.Schema
	DecisionKey   string          // the verdict's boolean: "actionable" | "needs_reply"
	CategoryKey   string          // the grouped category: "kind" | "ask_kind"
	PositiveLabel string          // the eval fixture's positive token
}

// ActionabilityContract is the shape SWT-22 shipped and SWT-23 shared. Named so
// the two lanes that use it point at ONE value rather than two equal literals —
// two literals is how "one contract" quietly becomes two.
var ActionabilityContract = Contract{
	SchemaName:    SchemaName,
	Schema:        VerdictSchema,
	DecisionKey:   "actionable",
	CategoryKey:   "kind",
	PositiveLabel: "actionable",
}

// InquiryContract is SWT-33's second contract.
var InquiryContract = Contract{
	SchemaName:    InquirySchemaName,
	Schema:        InquiryVerdictSchema,
	DecisionKey:   "needs_reply",
	CategoryKey:   "ask_kind",
	PositiveLabel: "needs_reply",
}

// Lane names one classify population: which inbox filter, which system prompt,
// which worker_type its rows are recorded under, which labelled set its eval
// defaults to, and which output Contract its verdicts take.
type Lane struct {
	Name          string // "personal" | "residue" | "inquiry"
	WorkerType    string // ai_runs.worker_type
	System        string // the system prompt
	PromptVersion string
	LabelsPath    string // default eval fixture
	Contract      Contract
}

// The two lanes. WorkerType differs because both inbox filters key their NOT
// EXISTS on it: ONE shared value would mean a residue message classified today,
// then claimed by a capture rule tomorrow into a local_only + ai_classify
// project, is PERMANENTLY invisible to the personal lane — already
// "classified", by a different prompt, with a verdict nobody would look for.
var (
	LanePersonal = Lane{
		Name:          "personal",
		WorkerType:    "classify",
		System:        SystemPrompt,
		PromptVersion: PromptVersion,
		LabelsPath:    "docs/evals/personal-actionability.jsonl",
		Contract:      ActionabilityContract,
	}
	LaneResidue = Lane{
		Name:          "residue",
		WorkerType:    "classify_residue",
		System:        ResidueSystemPrompt,
		PromptVersion: ResiduePromptVersion,
		LabelsPath:    "docs/evals/residue-actionability.jsonl",
		Contract:      ActionabilityContract,
	}
	// LaneInquiry (SWT-33) reads client conversation, not mail, and asks
	// whether a reply is owed. Its containment is NOT the ai_locality column —
	// collaboratory is 'any' — but cmd/classify's router, which is built with
	// NO general client at all, plus the lane's pinned restricted class.
	LaneInquiry = Lane{
		Name:          "inquiry",
		WorkerType:    "classify_inquiry",
		System:        InquirySystemPrompt,
		PromptVersion: InquiryPromptVersion,
		LabelsPath:    "docs/evals/inquiry-needs-reply.jsonl",
		Contract:      InquiryContract,
	}
)

// LaneByName resolves a --lane flag. Unknown names are refused with the two
// valid spellings, for the same reason the zero value is refused: a typo must
// never silently classify the residue with the personal prompt.
func LaneByName(name string) (Lane, error) {
	switch name {
	case LanePersonal.Name:
		return LanePersonal, nil
	case LaneResidue.Name:
		return LaneResidue, nil
	case LaneInquiry.Name:
		return LaneInquiry, nil
	default:
		return Lane{}, fmt.Errorf("unknown lane %q (want %q, %q or %q)",
			name, LanePersonal.Name, LaneResidue.Name, LaneInquiry.Name)
	}
}

// LaneByWorkerType resolves the lane a stored ai_runs row belongs to. Summarize
// takes a worker_type string (its signature is pinned by a structure test and
// its golden report uses a non-lane worker_type), so the lookup is by value and
// an unrecognised worker_type is NOT an error — it keeps today's behaviour.
func LaneByWorkerType(workerType string) (Lane, bool) {
	for _, l := range []Lane{LanePersonal, LaneResidue, LaneInquiry} {
		if l.WorkerType == workerType {
			return l, true
		}
	}
	return Lane{}, false
}

// validate refuses the zero value rather than defaulting it. A default would be
// the quietest possible bug: the run succeeds, the rows land, and the only
// symptom is a prompt asking whether a Nextdoor digest is a personal bill.
func (l Lane) validate() error {
	if l.Name == "" || l.WorkerType == "" || l.System == "" || l.PromptVersion == "" {
		return fmt.Errorf("classify: Config.Lane is unset or incomplete; pass LanePersonal, LaneResidue " +
			"or LaneInquiry — the zero lane is refused rather than defaulted, so the residue is never " +
			"classified by the personal prompt by omission")
	}
	return nil
}

// ResiduePromptVersion stamps every residue ai_runs.input. Distinct from the
// personal lane's stamp: without it, two runs that disagree are
// indistinguishable from a model that drifted.
const ResiduePromptVersion = "residue-v1"

// ResidueSystemPrompt is ONE prompt for every sender, same rule as the personal
// lane (per-sender prompts are rules in a costume). It deliberately does NOT
// open with the personal lane's "personal (non-work)" sentence: the residue is
// unclassified by definition — 1,287 of its messages are Upwork work
// conversations with no email address at all — so telling the model the mail is
// personal would be telling it something the census measured to be false. It
// also asks none of triage's questions: no ticket keys, no attach-vs-create,
// nothing about who the mail is for.
//
// Every not-actionable family below is named because the census says these ARE
// the residue (indeed 680, linkedin 695, nextdoor 726, github 455, amazon 407,
// medium 382, humblebundle 356...). The marketing-urgency trap paragraph is
// this lane's equivalent of the personal lane's near-miss clause, whose removal
// was MEASURED to flip the most common message shape in the corpus — do not
// trim it for brevity. The link paragraph is carried VERBATIM from the personal
// prompt: one spelling of the link contract, asserted structurally.
const ResidueSystemPrompt = `You classify one email from an uncategorised inbox for ACTIONABILITY.

Answer only about the message you are given. Messages may be in English or
Spanish; treat both identically.

actionable = true when the message requires the recipient to DO something, and
there is a consequence for not doing it:
  - a payment or bill that is due, or a balance that must be paid
  - a fine, a violation to cure, or a compliance deadline
  - an appointment to confirm, reschedule or attend
  - a document, form or signature that must be returned
  - an account-security action the recipient must take: verify a sign-in,
    reset a password, confirm a new device
  - a named human personally asking the recipient for something, as distinct
    from an automated template
  - anything with a stated deadline that has not passed

actionable = false for the bulk of this inbox, named plainly:
  - job alerts and recommended-jobs digests
  - social and professional-network notifications, digests and connection
    requests
  - repository and service notifications with no request addressed to the
    recipient (build and CI results, status pages)
  - newsletters, publications and reading digests
  - retail marketing, offers, sales and wishlist mail
  - shipping and delivery previews with nothing to do (a package merely on its
    way)

Marketing mail is written to look urgent, and that is the trap. "Ends tonight",
"final notice", "act now", "last chance", "your account needs attention", a
countdown — none of these is a consequence. Actionable means the recipient
loses something real by not acting, not that the sender wants a click.

RECALL IS THE OBJECTIVE. A missed payment or fine notice costs a late fee; a
false alarm costs one second to dismiss. When you are genuinely torn, answer
true.

When a numbered list of links is shown after the message, it is the complete
set of links available. Answer link_index with the number of the one link a
person would open to act on this message. Answer null when none of them is that
link, or when no list is shown at all — null is a normal answer, not a failure.
Never invent a number that is not in the list.

Fields:
  actionable  true or false, as above.
  kind        payment_due | deadline | appointment | action_required | informational
  title       one short line a human can act on, naming the amount and date ONLY
              if they appear in the message.
  reason      one sentence, citing the words that decided it.
  link_index  the number of the chosen link from the list, or null.`
