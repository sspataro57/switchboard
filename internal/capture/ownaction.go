package capture

// The own-action guard (SWT-45 review fix, docs/tickets/jira-activity-revive_SPEC.md
// J17) is a BACKSTOP FOR HIS OWN JIRA COMMENTS ONLY. With "Notify me about my
// own changes" on, Jira emails Salvador about his own changes as "Anonymous
// (JIRA)". That mail is inbound, so an activity rule would revive — or create —
// a task for something he just did.
//
// The Jira connector stores his own comments as direction='outbound' messages
// on the ticket's thread, `jira:{site_host}:{KEY}`. On prod 26 of the 208
// "Anonymous (JIRA)" Treetop emails correlate with such a message, each sent
// between OwnActionLead before and OwnActionLag after the email (verified
// read-only). So an activity match whose ticket thread holds an outbound
// message in that window is his own comment: the revive and the creation are
// skipped, deterministically, with no model.
//
// WHAT IT DOES NOT COVER. The other 182 Anonymous emails are field/status-edit
// notices with no comment (146 have no outbound message even in [-60m, +10m]),
// and the connector stores no changelog, so nothing identifies an edit's actor.
// Those still revive or create. The protection against his own EDITS is the
// owner turning Jira's "Notify me about my own changes" off (SPEC J17).
//
// THE RACE. Capture can decide the email before the Jira poller has stored the
// comment. A thread not yet known to be synced past the window must not fail
// open silently, so the message is DEFERRED — no decision row is written, and it
// stays pending for the next capture pass — for at most OwnActionMaxWait from
// its ingest. Past that the revive proceeds, and the decision reason says the
// guard ran blind.
//
// THE NAMED-ACTOR EXEMPTION (second review batch). Jira's notification From is
// `"<Name> (JIRA)" <jira@site>`. His own changes arrive as "Anonymous (JIRA)"
// (all 26 comment correlations on prod); no "(JIRA)"-shaped mail has ever
// carried his own name as the actor. Atlassian Cloud sites use another shape,
// `<Name> <jira@site>` (prod message 31058 names him that way, from Foundry's
// site, which no poller covers): that shape falls to the window check, so on
// such a site the exemption does not apply. The template words below are the
// only sender literals,
// and they only EXEMPT: the placeholder never suppresses anything by itself.
// A NAMED actor who is not him positively identifies someone else's action:
// prod email 158474, "Katie Evans mentioned you on WEB-10355", sent 20:27:23Z,
// 8.6 minutes after his own outbound comment on that ticket (20:18:45Z) — the
// window alone would have suppressed exactly the mention he wants revived. So
// such an email proceeds at once: no window verdict, no freshness reads, no
// wait. "Not him" means: not Jira's anonymous placeholder, and not the stored
// sender of the outbound message the window found (his Jira display name as
// the connector stores it). An Anonymous email, or a From with no "(JIRA)"
// shape, keeps the window check.
//
// This file is pure: the window, the freshness instant, the From parse and the
// verdict are functions of values. The reads live in rules_store.go
// (ownActionFacts).

import (
	"strings"
	"time"
)

const (
	// OwnActionLead is how long BEFORE the notification email his own Jira
	// action may be: the comment is written, then Jira sends the mail.
	OwnActionLead = 10 * time.Minute
	// OwnActionLag is how long AFTER the email's Date his action may be stamped
	// (Jira's clock and its mail server's are not the same clock).
	OwnActionLag = 2 * time.Minute
	// OwnActionSyncMargin is added past the window before a poller run counts
	// as having seen it: Jira's search index trails a comment by seconds, and
	// sync_runs.started_at is the database's clock, not Jira's.
	OwnActionSyncMargin = 2 * time.Minute
	// OwnActionMaxWait bounds the deferral, measured from the message's ingest
	// (normalized_messages.created_at). Two connector-jira ticks at */15.
	OwnActionMaxWait = 30 * time.Minute
)

// ownActionWindow is the ONE spelling of the window, inclusive at both ends:
// an outbound message on the ticket's thread with sent_at in [from, to] is
// his own action. The SQL binds these two values; it never re-derives them.
func ownActionWindow(sent time.Time) (from, to time.Time) {
	return sent.Add(-OwnActionLead), sent.Add(OwnActionLag)
}

// ownActionSyncedPast is the instant a poller run must have STARTED after for
// the thread to be known synced past the window's far edge.
func ownActionSyncedPast(sent time.Time) time.Time {
	return sent.Add(OwnActionLag + OwnActionSyncMargin)
}

// Jira's notification template wording, as it appears in the From display
// name. These are Jira's strings, not ours: the template renders the acting
// user as `<Name> (JIRA)`, and renders a user whose name it withholds (his
// own changes, on prod) as the placeholder `Anonymous (JIRA)`.
const (
	jiraActorSuffix    = "(JIRA)"
	jiraAnonymousActor = "Anonymous"
)

// jiraNotificationActor reads the acting user's name out of a From value with
// Jira's notification shape: `"<Name> (JIRA)" <addr>`, quoted or not, or the
// bare display name. ok is false when there is no "(JIRA)" shape at all — a
// bare address, "Jira <jira@…>", another source's mail. The placeholder is
// returned as a name (ok true); namedJiraActor is what excludes it. Only the
// LAST "(JIRA)" is the template's, so a name that itself contains parentheses
// ("Katie (QA) Evans") survives whole.
func jiraNotificationActor(from string) (string, bool) {
	s := strings.TrimSpace(from)
	if strings.HasSuffix(s, ">") {
		if i := strings.LastIndex(s, "<"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		s = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s[1 : len(s)-1])
		s = strings.TrimSpace(s)
	}
	if !strings.HasSuffix(s, jiraActorSuffix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimSuffix(s, jiraActorSuffix))
	if name == "" {
		return "", false
	}
	return name, true
}

// namedJiraActor is the email's acting user when Jira NAMED one: the
// "(JIRA)" shape with a name other than the anonymous placeholder. "" means
// the guard cannot tell whose action it is from the From alone.
func namedJiraActor(from string) string {
	name, ok := jiraNotificationActor(from)
	if !ok || sameJiraActor(name, jiraAnonymousActor) {
		return ""
	}
	return name
}

// sameJiraActor compares two display names the way a human reads them: case
// and runs of whitespace do not make a different person.
func sameJiraActor(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// The guard's verdicts.
const (
	ownActionNotApplicable = "not_applicable" // no jira poller covers the key: no thread can hold his comment
	ownActionNamed         = "named_actor"    // Jira names someone other than him as the actor: proceed now
	ownActionClear         = "clear"          // synced past the window, nothing of his in it: proceed
	ownActionSkip          = "own_action"     // an outbound message in the window: skip the revive/creation
	ownActionDefer         = "deferred"       // not yet synced, inside the wait bound: decide nothing yet
	ownActionBlind         = "blind"          // not synced, past the bound: proceed, and say so
)

// ownActionObservation is what the guard may know, as values.
type ownActionObservation struct {
	pollers int    // provider='jira' accounts whose scopes claim the key's prefix
	actor   string // namedJiraActor of the email's From ("" = anonymous or no Jira shape)
	found   bool   // an outbound message on one of their threads inside the window
	hisName string // that outbound message's stored sender (his Jira display name)
	fresh   bool   // every such poller ran after ownActionSyncedPast and has nothing of K unnormalized
	// waited is database now() minus the message's ingest.
	waited time.Duration
}

// namedOther: Jira named an actor, and it is not the author of the outbound
// message the window found (with nothing found, any named actor is other).
func (o ownActionObservation) namedOther() bool {
	return o.actor != "" && !(o.found && sameJiraActor(o.actor, o.hisName))
}

// ownActionSettled is the verdict the lookup facts alone decide — pollers,
// the From's actor and the window — with ok false when only freshness can
// decide. The store reads freshness (sync_runs, raw rows, the ingest clock)
// only when ok is false, so a named-other email never waits and never reads it.
func ownActionSettled(o ownActionObservation) (string, bool) {
	switch {
	case o.pollers == 0:
		return ownActionNotApplicable, true
	case o.namedOther():
		return ownActionNamed, true
	case o.found:
		// An outbound message in the window decides even on a stale thread:
		// what is already stored cannot un-happen.
		return ownActionSkip, true
	}
	return "", false
}

// decideOwnAction is the guard's verdict, in order: no poller; a named actor
// who is not him; his outbound message in the window; then freshness, the
// deferral and its bound.
func decideOwnAction(o ownActionObservation) string {
	if v, ok := ownActionSettled(o); ok {
		return v
	}
	switch {
	case o.fresh:
		return ownActionClear
	case o.waited < OwnActionMaxWait:
		return ownActionDefer
	default:
		return ownActionBlind
	}
}
