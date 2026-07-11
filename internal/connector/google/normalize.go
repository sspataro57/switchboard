// Package google is the Google connector (SPEC 07-google-oauth-pollers):
// OAuth token plumbing, Gmail + Calendar pollers in the upworkcrm raw-first
// shape, cross-account Message-ID dedup, and the pure normalizers.
package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Channel value for every Gmail message.
const Channel = "gmail"

// NormalizedMessage is the canonical projection of one raw Gmail message.
type NormalizedMessage struct {
	ThreadKey         string
	ExternalMessageID string
	Direction         string
	SentAt            time.Time
	Subject           string
	Sender            string
	BodyText          string
	Channel           string
	GmailMessageID    string
	GmailThreadID     string
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

type gmailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	InternalDate string   `json:"internalDate"`
	Payload      struct {
		MimeType string        `json:"mimeType"`
		Headers  []gmailHeader `json:"headers"`
		Body     struct {
			Data string `json:"data"`
		} `json:"body"`
		Parts []gmailPart `json:"parts"`
	} `json:"payload"`
}

// NormalizeGmailMessage is the pure mapper from one format=full raw message.
// accountEmail names the mailbox this copy came from (thread-key qualifier);
// ownEmails is the set of ALL provider='google' account emails — direction is
// outbound iff the From address is one of ours (criterion 8, invariant 5).
func NormalizeGmailMessage(raw json.RawMessage, accountEmail string, ownEmails map[string]bool) (NormalizedMessage, error) {
	var m gmailMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return NormalizedMessage{}, fmt.Errorf("parse raw gmail message: %w", err)
	}
	if m.ID == "" {
		return NormalizedMessage{}, fmt.Errorf("raw gmail message has no id")
	}

	headers := map[string]string{}
	for _, h := range m.Payload.Headers {
		headers[strings.ToLower(h.Name)] = h.Value
	}

	externalID := strings.TrimSpace(headers["message-id"])
	if externalID == "" {
		externalID = "gmail:" + m.ID
	}

	sentAt := time.Time{}
	if ms, err := strconv.ParseInt(m.InternalDate, 10, 64); err == nil {
		sentAt = time.UnixMilli(ms).UTC()
	}

	direction := "inbound"
	if addr := extractAddress(headers["from"]); addr != "" && ownEmails[strings.ToLower(addr)] {
		direction = "outbound"
	}

	body := firstTextPlain(gmailPart{
		MimeType: m.Payload.MimeType,
		Body:     m.Payload.Body,
		Parts:    m.Payload.Parts,
	})
	if body == "" {
		body = m.Snippet
	}

	return NormalizedMessage{
		ThreadKey:         "gmail:" + accountEmail + ":" + m.ThreadID,
		ExternalMessageID: externalID,
		Direction:         direction,
		SentAt:            sentAt,
		Subject:           headers["subject"],
		Sender:            headers["from"],
		BodyText:          body,
		Channel:           Channel,
		GmailMessageID:    m.ID,
		GmailThreadID:     m.ThreadID,
	}, nil
}

// extractAddress pulls the bare email from a possibly "Name <addr>" header,
// case-insensitively usable against the own-account set.
func extractAddress(from string) string {
	if from == "" {
		return ""
	}
	if a, err := mail.ParseAddress(from); err == nil {
		return a.Address
	}
	return strings.Trim(from, "<> ")
}

// firstTextPlain walks the MIME tree depth-first for the first text/plain
// leaf and decodes its base64url (unpadded) body.
func firstTextPlain(p gmailPart) string {
	if strings.HasPrefix(p.MimeType, "text/plain") && p.Body.Data != "" {
		if decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(p.Body.Data, "=")); err == nil {
			return string(decoded)
		}
	}
	for _, child := range p.Parts {
		if body := firstTextPlain(child); body != "" {
			return body
		}
	}
	return ""
}

// Attendee is one calendar attendee projection.
type Attendee struct {
	Email          string `json:"email"`
	ResponseStatus string `json:"response_status"`
	Organizer      bool   `json:"organizer"`
	Self           bool   `json:"self"`
}

// NormalizedEvent is the canonical projection of one raw calendar event.
type NormalizedEvent struct {
	StartsAt     time.Time
	EndsAt       time.Time
	Title        string
	Status       string
	Transparency string
	AllDay       bool
	Attendees    []Attendee
}

type calTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
}

type calEvent struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Summary      string  `json:"summary"`
	Transparency string  `json:"transparency"`
	Start        calTime `json:"start"`
	End          calTime `json:"end"`
	Attendees    []struct {
		Email          string `json:"email"`
		ResponseStatus string `json:"responseStatus"`
		Organizer      bool   `json:"organizer"`
		Self           bool   `json:"self"`
	} `json:"attendees"`
}

// NormalizeCalendarEvent is the pure mapper from one raw calendar event.
// Absent transparency means opaque (Google semantics, criterion 10).
func NormalizeCalendarEvent(raw json.RawMessage) (NormalizedEvent, error) {
	var e calEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return NormalizedEvent{}, fmt.Errorf("parse raw calendar event: %w", err)
	}
	if e.ID == "" {
		return NormalizedEvent{}, fmt.Errorf("raw calendar event has no id")
	}

	ne := NormalizedEvent{
		Title:        e.Summary,
		Status:       e.Status,
		Transparency: e.Transparency,
	}
	if ne.Transparency == "" {
		ne.Transparency = "opaque"
	}

	var err error
	ne.StartsAt, ne.AllDay, err = parseCalTime(e.Start)
	if err != nil {
		return NormalizedEvent{}, fmt.Errorf("parse event start: %w", err)
	}
	ne.EndsAt, _, err = parseCalTime(e.End)
	if err != nil {
		return NormalizedEvent{}, fmt.Errorf("parse event end: %w", err)
	}

	for _, a := range e.Attendees {
		ne.Attendees = append(ne.Attendees, Attendee{
			Email: a.Email, ResponseStatus: a.ResponseStatus, Organizer: a.Organizer, Self: a.Self,
		})
	}
	return ne, nil
}

func parseCalTime(t calTime) (time.Time, bool, error) {
	switch {
	case t.DateTime != "":
		parsed, err := time.Parse(time.RFC3339, t.DateTime)
		return parsed, false, err
	case t.Date != "":
		parsed, err := time.Parse("2006-01-02", t.Date)
		return parsed, true, err
	default:
		return time.Time{}, false, fmt.Errorf("event time has neither date nor dateTime")
	}
}

// Normalize is the second phase: it reads ONLY raw_source_items, loads the
// own-email set once, dedups gmail messages by Message-ID, and upserts
// events. Losing dedup copies are still stamped normalized_at.
func Normalize(ctx context.Context, sink *PGSink, cfg Config) (Stats, error) {
	var stats Stats

	ownEmails, err := sink.ownEmailSet(ctx)
	if err != nil {
		return stats, fmt.Errorf("load own-email set: %w", err)
	}

	items, err := sink.pendingRaw(ctx, cfg.All)
	if err != nil {
		return stats, fmt.Errorf("list raw items: %w", err)
	}

	for _, it := range items {
		switch {
		case strings.HasPrefix(it.externalID, "gmail:"):
			nm, err := NormalizeGmailMessage(it.raw, it.accountEmail, ownEmails)
			if err != nil {
				return stats, fmt.Errorf("normalize %s: %w", it.externalID, err)
			}
			deduped, err := sink.upsertMessage(ctx, it.id, nm)
			if err != nil {
				return stats, fmt.Errorf("apply %s: %w", it.externalID, err)
			}
			if deduped {
				stats.DedupSkipped++
			}
		case strings.HasPrefix(it.externalID, "calendar:"):
			ne, err := NormalizeCalendarEvent(it.raw)
			if err != nil {
				return stats, fmt.Errorf("normalize %s: %w", it.externalID, err)
			}
			if err := sink.upsertEvent(ctx, it.id, ne); err != nil {
				return stats, fmt.Errorf("apply %s: %w", it.externalID, err)
			}
		default:
			return stats, fmt.Errorf("raw item %d has unknown external_id shape %q", it.id, it.externalID)
		}
		if err := sink.markNormalized(ctx, it.id); err != nil {
			return stats, fmt.Errorf("stamp normalized_at for %s: %w", it.externalID, err)
		}
		stats.Normalized++
	}
	return stats, nil
}
