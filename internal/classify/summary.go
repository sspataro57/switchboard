package classify

// Summarize owns the classify report's SQL and folds (SWT-29 criterion 11):
// the verdict query, the four-way link-state fold and the skipped-run fold all
// MOVED here from report.go, so the /funnel page and `classify report` read
// ONE query and print the same numbers for the same window. ReportForWorker is
// now a renderer over Summary; its printed text — including the no-fallback
// note the structure tests scan for — stays in report.go, byte-identical.
//
// SWT-33 adds the INQUIRY lane's folds here for the same reason: the three
// open/answered states and the by-channel breakdown are computed ONCE, so the
// CLI report and /funnel cannot disagree. The lane is resolved from the
// worker_type Summarize is already given (LaneByWorkerType), so the signature —
// pinned by ReportForWorker's structure test and the golden report — does not
// move, and an unrecognised worker_type keeps the actionability fold.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// summaryFlagCap bounds Summary.Flags (criterion 13): 50 fits a page and stays
// scannable (capture's reportListLimit exists for the same reason). The COUNTS
// are never capped — capping Flagged would make a bad day look normal.
const summaryFlagCap = 50

// The three states of a FLAGGED inquiry verdict (SWT-33 criterion 17), one
// spelling for every renderer. They are never collapsed: `answered in thread`
// is a later outbound on a thread-EXACT key, which really is a reply in that
// thread; `spoke in conversation since` is a later outbound anywhere in an
// unthreaded Slack channel or DM, which in a busy channel says little about
// THIS question (one conversation-level key held 9,704 messages on
// 2026-09-10). Folding the second into the first would hide real open
// inquiries behind a channel Salvador happened to speak in.
const (
	StateOpen             = "open"
	StateAnsweredInThread = "answered in thread"
	StateSpokeSince       = "spoke in conversation since"
)

// unrecordedChannel groups counts whose row carries no channel — skip rows
// written before SWT-33 recorded one. Its own row, so the by-channel breakdown
// still sums to the lane totals.
const unrecordedChannel = "(unrecorded)"

// repliedSinceSQL is the READ-TIME "still open" fold (SWT-33 criteria 17-18):
// does the verdict's recorded thread carry an OUTBOUND message sent after the
// classified message? It is the one deliberate join from a verdict back to
// normalized_messages — a statement about the world NOW, not about what was
// classified, so it reads thread_id, direction and sent_at and nothing else —
// and it writes nothing: the verdict row is never updated, so the rule is
// tunable without re-running the GPU and a mis-tuned rule loses no data.
//
// Its evidence is `direction = 'outbound'`, invariant 5's own marker. That
// column was MEASURED to carry values on the armed project's threads before
// this shipped (runbook, "The inquiry lane"), and the integration suite proves
// the query reads it by mutating the seeded row three ways.
//
// SET-BASED ON PURPOSE: the latest outbound instant per thread, computed once,
// then compared with the classified message's sent_at. The first cut was a
// correlated EXISTS per verdict, and normalized_messages has no index on
// thread_id — the same shape measured 21.5 s over 1,806 verdicts on the
// production db (2026-09-10), a full scan per row. This is one scan per report.
// "Some outbound after t" and "the latest outbound is after t" are the same
// predicate.
//
// repliedSinceCol is the column it produces. Both halves are named constants
// so the structure test can hold them to an ALLOWLIST of identifiers — the
// aliases t and lo appear nowhere else, and nothing but thread_id, direction
// and sent_at is read from the table (t.id is the join key to the verdict).
//
// TIES FAIL CLOSED (Codex adversarial re-review, 2026-09-10): "later" means a
// strictly later sent_at, exactly as criterion 17 wrote it, so a reply stamped
// in the same instant as the question does NOT answer it. The first review
// round asked for a (sent_at, id) tie-break; the second showed why that is
// wrong — the id is a BIGSERIAL insertion key, so a backfilled or
// late-ingested OLDER message gets a HIGHER id, and on a timestamp tie that
// would mark a real open inquiry answered and hide it. Reading "open" when
// unsure is the safe error: the review surface shows one extra line, never one
// fewer. max() ignores NULL stamps, so a NULL-stamped outbound can never be
// "the latest".
const repliedSinceCol = `COALESCE(lo.last_outbound > t.sent_at, false)`

const repliedSinceSQL = `
	      LEFT JOIN normalized_messages t
	             ON t.id = (e.fields->>'normalized_message_id')::bigint
	      LEFT JOIN (SELECT thread_id, max(sent_at) AS last_outbound
	                   FROM normalized_messages
	                  WHERE direction = 'outbound' AND thread_id IS NOT NULL
	                  GROUP BY thread_id) lo
	             ON lo.thread_id = NULLIF(e.fields->>'thread_id', '')::bigint`

// Summary is one lane's shadow-classification numbers for a window.
type Summary struct {
	WorkerType          string
	Classified, Flagged int
	ByKind              map[string]int
	LinkResolved        int
	LinkDeclined        int
	LinkNoneOffered     int
	LinkRejected        int
	Skipped             int
	ByAvailReason       map[string]int
	ByClassReason       map[string]int
	// The INQUIRY lane's three states (SWT-33 criterion 17); zero on the other
	// two lanes. Every flagged inquiry verdict is in exactly one of them:
	// Flagged = Open + AnsweredInThread + SpokeInConversationSince.
	Open                     int
	AnsweredInThread         int
	SpokeInConversationSince int
	// ByChannel breaks every inquiry count down by the channel STORED on the
	// verdict (criterion 30) — never re-joined from normalized_messages, because
	// what was classified is what should be shown. It is what buys back the
	// rejected --channel flag: a bad number still says which message shape
	// broke. Empty on the other two lanes.
	ByChannel map[string]ChannelCounts
	Flags     []Flag // newest first, capped at summaryFlagCap (the page's list)
	// allFlags is EVERY flagged verdict, uncapped. The CLI report renders all
	// of them — its text is byte-identical to the pre-refactor output
	// (criterion 12), and a residue window can carry a thousand-plus flagged
	// lines that a silent 50-row truncation would eat. Unexported on purpose:
	// the dashboard gets the capped list, the in-package renderer gets the
	// truth.
	allFlags []Flag
}

// ChannelCounts is one channel's row of the inquiry breakdown. The rows sum to
// the lane totals — a breakdown that does not add up is a second set of numbers
// nobody can reconcile.
type ChannelCounts struct {
	Classified, Flagged                              int
	Open, AnsweredInThread, SpokeInConversationSince int
	Skipped                                          int
}

// Flag is one flagged verdict. Sender and subject come from the STORED
// ai_extractions.fields, exactly as the CLI report reads them (criterion 13) —
// never a join back to normalized_messages for a second copy of what the model
// was shown.
//
// On the inquiry lane Kind carries ask_kind and Title the model's `ask` line,
// and the last four fields are set; they are empty on the other two lanes.
type Flag struct {
	At        time.Time
	MessageID int64
	Kind      string
	Sender    string
	Subject   string
	Title     string
	LinkURL   string

	Channel     string
	Asker       string
	ThreadScope string
	State       string
}

// Summarize computes one lane's Summary. since == 0 means the whole history.
func Summarize(ctx context.Context, pool *pgxpool.Pool, since time.Duration, workerType string) (Summary, error) {
	s := Summary{
		WorkerType:    workerType,
		ByKind:        map[string]int{},
		ByAvailReason: map[string]int{},
		ByClassReason: map[string]int{},
		ByChannel:     map[string]ChannelCounts{},
	}
	lane, known := LaneByWorkerType(workerType)
	inquiry := known && lane.Name == LaneInquiry.Name

	// The replied-since column is computed for the inquiry lane only; the other
	// lanes select a constant so one scan serves all three.
	replied, joins := `false`, ``
	if inquiry {
		replied, joins = repliedSinceCol, repliedSinceSQL
	}
	q := `SELECT e.fields, r.created_at, ` + replied + `
	      FROM ai_extractions e
	      JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type=$1 AND r.status='ok'` + joins
	args := []any{workerType}
	if since > 0 {
		args = append(args, since.String())
		q += ` WHERE r.created_at >= now() - $2::interval`
	}
	q += ` ORDER BY r.created_at DESC`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return s, fmt.Errorf("select verdicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var raw []byte
		var createdAt time.Time
		var repliedSince bool
		if err := rows.Scan(&raw, &createdAt, &repliedSince); err != nil {
			return s, fmt.Errorf("scan verdict: %w", err)
		}
		var f struct {
			Actionable     bool     `json:"actionable"`
			Kind           string   `json:"kind"`
			Title          string   `json:"title"`
			Reason         string   `json:"reason"`
			Sender         string   `json:"sender"`
			Subject        string   `json:"subject"`
			MessageID      int64    `json:"normalized_message_id"`
			LinkCandidates *int     `json:"link_candidates"`
			LinkURL        *string  `json:"link_url"`
			LinkRejectedAs *float64 `json:"link_index_rejected"`
			// The inquiry contract (SWT-33).
			NeedsReply  bool   `json:"needs_reply"`
			AskKind     string `json:"ask_kind"`
			Asker       string `json:"asker"`
			Ask         string `json:"ask"`
			Channel     string `json:"channel"`
			ThreadScope string `json:"thread_scope"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		// The lane's contract decides which keys are the decision and the
		// category (D1): reading `actionable` off an inquiry verdict decodes
		// to false on every row, which would print a lane that never flags.
		decision, category, title := f.Actionable, f.Kind, f.Title
		if inquiry {
			decision, category, title = f.NeedsReply, f.AskKind, f.Ask
		}
		channel := channelKey(f.Channel)

		s.Classified++
		s.ByKind[category]++
		if inquiry {
			s.bump(channel, func(c *ChannelCounts) { c.Classified++ })
		}
		// The four link states of SWT-25 criterion 21, counted separately: a
		// counter that cannot tell "nothing to offer" from "the model declined"
		// from "the model answered nonsense" is an alarm nobody can read. Rows
		// predating SWT-25 carry no link_candidates and are not counted at all.
		switch {
		case f.LinkCandidates == nil:
			// pre-SWT-25 verdict, or an inquiry verdict (no link contract)
		case f.LinkURL != nil && *f.LinkURL != "":
			s.LinkResolved++
		case f.LinkRejectedAs != nil:
			s.LinkRejected++
		case *f.LinkCandidates == 0:
			s.LinkNoneOffered++
		default:
			s.LinkDeclined++
		}
		if !decision {
			continue
		}
		s.Flagged++
		link := ""
		if f.LinkURL != nil {
			link = *f.LinkURL
		}
		fl := Flag{
			At: createdAt, MessageID: f.MessageID, Kind: category,
			Sender: f.Sender, Subject: f.Subject, Title: title, LinkURL: link,
		}
		if inquiry {
			fl.Channel, fl.Asker, fl.ThreadScope = f.Channel, f.Asker, f.ThreadScope
			fl.State = inquiryState(f.ThreadScope, repliedSince)
			s.countState(channel, fl.State)
		}
		s.allFlags = append(s.allFlags, fl)
		if len(s.Flags) < summaryFlagCap {
			s.Flags = append(s.Flags, fl)
		}
	}
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("iterate verdicts: %w", err)
	}

	if err := summarizeSkipped(ctx, pool, since, workerType, inquiry, &s); err != nil {
		return s, err
	}
	return s, nil
}

// inquiryState places one flagged inquiry verdict in exactly one of the three
// states. A replied-since verdict of scope `none` cannot occur (a message with
// no thread has nothing to be replied in) and would stay open if it did — the
// fold never upgrades a claim the data does not carry.
func inquiryState(scope string, repliedSince bool) string {
	switch {
	case repliedSince && scope == ScopeThread:
		return StateAnsweredInThread
	case repliedSince && scope == ScopeConversation:
		return StateSpokeSince
	default:
		return StateOpen
	}
}

// countState adds one flagged verdict to the lane counters and its channel row.
func (s *Summary) countState(channel, state string) {
	s.bump(channel, func(c *ChannelCounts) {
		c.Flagged++
		switch state {
		case StateAnsweredInThread:
			c.AnsweredInThread++
		case StateSpokeSince:
			c.SpokeInConversationSince++
		default:
			c.Open++
		}
	})
	switch state {
	case StateAnsweredInThread:
		s.AnsweredInThread++
	case StateSpokeSince:
		s.SpokeInConversationSince++
	default:
		s.Open++
	}
}

func (s *Summary) bump(channel string, fn func(*ChannelCounts)) {
	c := s.ByChannel[channel]
	fn(&c)
	s.ByChannel[channel] = c
}

func channelKey(channel string) string {
	if channel == "" {
		return unrecordedChannel
	}
	return channel
}

// summarizeSkipped folds the lane the extraction join above CANNOT see. A
// refused message writes no extraction at all — that is what keeps "no
// permitted provider looked" structurally different from "the model looked and
// found nothing"; without this fold a fully-skipped pass summarizes as
// classified: 0, indistinguishable from an empty inbox or a dead poller.
//
// byChannel (the inquiry lane) also files each skip under its channel: the
// aggregate row carries a `channels` breakdown and a per-message row its
// `channel`; a row carrying neither predates SWT-33 and is filed as
// unrecorded, so the channel rows still sum to Skipped.
func summarizeSkipped(ctx context.Context, pool *pgxpool.Pool, since time.Duration, workerType string,
	byChannel bool, s *Summary) error {
	q := `SELECT input FROM ai_runs WHERE worker_type=$1 AND status='skipped'`
	args := []any{workerType}
	if since > 0 {
		args = append(args, since.String())
		q += ` AND created_at >= now() - $2::interval`
	}
	q += ` ORDER BY created_at`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("select skipped runs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan skipped run: %w", err)
		}
		var rec struct {
			AvailReason  string         `json:"avail_reason"`
			AvailReasons map[string]int `json:"avail_reasons"`
			ClassReasons map[string]int `json:"class_reasons"`
			SkippedCount *int           `json:"skipped_count"`
			Channels     map[string]int `json:"channels"`
			Channel      string         `json:"channel"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		n := 1
		if rec.SkippedCount != nil {
			n = *rec.SkippedCount
		}
		s.Skipped += n
		// Prefer the breakdown when the row carries one: a pass can refuse for
		// more than one reason, and filing the whole count under the dominant
		// one prints a wrong number in the one place an operator looks.
		if len(rec.AvailReasons) > 0 {
			for k, v := range rec.AvailReasons {
				s.ByAvailReason[k] += v
			}
		} else {
			reason := rec.AvailReason
			if reason == "" {
				reason = "unrecorded"
			}
			s.ByAvailReason[reason] += n
		}
		for k, v := range rec.ClassReasons {
			s.ByClassReason[k] += v
		}
		if byChannel {
			switch {
			case len(rec.Channels) > 0:
				for k, v := range rec.Channels {
					s.bump(channelKey(k), func(c *ChannelCounts) { c.Skipped += v })
				}
			default:
				s.bump(channelKey(rec.Channel), func(c *ChannelCounts) { c.Skipped += n })
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate skipped runs: %w", err)
	}
	return nil
}
