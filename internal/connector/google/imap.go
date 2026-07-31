package google

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// This file holds the IMAP source contract for SWT-11: the interface the ingest
// phase talks to, the value types crossing it, and the folder-selection rules.
//
// The interface is deliberately READ-ONLY. Criterion 6 says this connector never
// marks mail read, never relocates, removes or labels — and the cheapest way to
// guarantee that is to give the abstraction no verb that could. It exposes no
// mutating operation of any kind, and none may be added: an implementation
// cannot change a mailbox through a method that does not exist. A test greps
// this file for such verbs, so do not name them here either.
//
// The single write operation against any mail server in this connector is the
// SMTP submission in smtp.go, reachable only from an approved delivery row
// (invariant 4).

// DefaultMaxMessageBytes caps how much of one message the source fetches.
//
// Above it the source returns headers only with Truncated set, which still
// carries Message-ID, From, Subject and the threading headers — everything
// normalization needs except the body. The point is that one 40 MiB attachment
// cannot stall a pass or bloat raw_source_items; the mailboxes in scope hold
// ~117k messages between them.
const DefaultMaxMessageBytes = 1 << 20 // 1 MiB

// MaxMessageBytes reads the MAIL_MAX_MESSAGE_BYTES override.
//
// Unparseable or non-positive falls back to the default rather than erroring:
// a typo in a Deployment env must not silently produce a zero cap, which would
// truncate EVERY message to headers and quietly empty every body in the funnel.
func MaxMessageBytes() int {
	raw := os.Getenv("MAIL_MAX_MESSAGE_BYTES")
	if raw == "" {
		return DefaultMaxMessageBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultMaxMessageBytes
	}
	return n
}

// Folder is one selectable mailbox as LIST reports it.
type Folder struct {
	// Name is the server-side mailbox name, used verbatim in external_id.
	Name string
	// UIDValidity is the mailbox's generation. A change invalidates every stored
	// UID for the folder and forces a resync (criterion 8).
	UIDValidity uint32
	// Sent marks the RFC 6154 \Sent SPECIAL-USE mailbox — the reliable
	// cross-provider way to find the Sent folder, which matters because Sent is
	// the own-message loop-closure surface (invariant 5).
	Sent bool
}

// SearchCriteria narrows a folder scan. Zero values mean "unbounded", but the
// ingest phase always sets one of them — an unbounded SEARCH against a
// 106,930-message mailbox is the thing criterion 7 exists to prevent.
type SearchCriteria struct {
	// Since maps to IMAP SEARCH SINCE (the backfill window on a first pass).
	Since time.Time
	// FromUID maps to a UID range scan, {FromUID}:* on an incremental pass.
	FromUID uint32
}

// FetchedMessage is one message as the source hands it back.
type FetchedMessage struct {
	UID          uint32
	InternalDate time.Time
	Flags        []string
	// Size is the server-reported RFC822.SIZE, which is what the cap is judged
	// against — so an oversized message is never transferred in full.
	Size int
	// Truncated marks a headers-only capture (Size exceeded the cap).
	Truncated bool
	// RFC822 is the message bytes exactly as received: 8-bit-clean, never
	// re-encoded, which is why the raw envelope carries them base64.
	RFC822 []byte
}

// MailSource is the read-only mail server contract. See the file comment for why
// it has no write verb.
type MailSource interface {
	// Folders lists selectable mailboxes with their UIDVALIDITY and attributes.
	Folders(ctx context.Context) ([]Folder, error)
	// Search returns matching UIDs in ascending order.
	Search(ctx context.Context, folder string, crit SearchCriteria) ([]uint32, error)
	// Fetch returns the messages for uids. It MUST use BODY.PEEK, which is what
	// keeps a read from marking the message as having been read.
	// maxBytes>0 caps a message to its headers with Truncated set.
	Fetch(ctx context.Context, folder string, uids []uint32, maxBytes int) ([]FetchedMessage, error)
	// Idle blocks until the folder changes, then signals. Watch mode only; the
	// signal is a wake-up, never trusted as a payload — the caller re-fetches by
	// UID, the same discipline the orchestrator applies to Postgres NOTIFY.
	Idle(ctx context.Context, folder string) (<-chan struct{}, error)
}

// FolderCursor is the per-folder ingest position, stored under
// sync_cursor.imap_folders. UIDNext is only advanced after every message in a
// pass is durably in raw_source_items (criterion 8), so a crash re-fetches
// rather than skips.
type FolderCursor struct {
	UIDValidity uint32 `json:"uidvalidity"`
	UIDNext     uint32 `json:"uid_next"`
}

// Well-known Gmail folder names used as the last-resort Sent fallback.
const (
	InboxFolder     = "INBOX"
	gmailSentFolder = "[Gmail]/Sent Mail"
)

// SelectFolders picks the mailboxes to ingest: INBOX plus Sent, and nothing else
// (criterion 9).
//
// Sent is not optional decoration — it is the own-message loop-closure surface
// (invariant 5). Without it our own replies never re-enter, deliveries stay
// unconfirmed, and SWT-16's capture pass would then report them as sent by hand.
//
// Resolution order, most to least reliable: an explicit override (an operator
// naming folders knows more than a heuristic, and it is the escape hatch for a
// server whose SPECIAL-USE attributes are missing or wrong), then the RFC 6154
// \Sent marker, then Gmail's conventional name.
//
// Spam, Trash, All Mail and Drafts are excluded deliberately: All Mail alone
// would duplicate the entire mailbox, and Drafts holds messages that were never
// sent, which must never look like correspondence.
func SelectFolders(all []Folder, override []string) []Folder {
	byName := make(map[string]Folder, len(all))
	for _, f := range all {
		byName[f.Name] = f
	}

	var out []Folder
	seen := map[string]bool{}
	add := func(f Folder) {
		if f.Name == "" || seen[f.Name] {
			return
		}
		seen[f.Name] = true
		out = append(out, f)
	}

	if len(override) > 0 {
		for _, name := range override {
			if f, ok := byName[strings.TrimSpace(name)]; ok {
				add(f)
			}
		}
		return out
	}

	if f, ok := byName[InboxFolder]; ok {
		add(f)
	}
	for _, f := range all {
		if f.Sent {
			add(f)
			return out
		}
	}
	if f, ok := byName[gmailSentFolder]; ok {
		add(f)
	}
	return out
}

// FoldersFromEnv reads the MAIL_FOLDERS comma-separated override.
func FoldersFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("MAIL_FOLDERS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		if n := strings.TrimSpace(name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// MailHosts is the per-account IMAP/SMTP endpoint configuration. Empty fields
// take the Gmail defaults, which is what every mailbox in scope uses.
type MailHosts struct {
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

// Defaults for Gmail. 993 is the only port Gmail's IMAP supports (implicit TLS);
// 587 is submission with STARTTLS.
const (
	DefaultIMAPHost = "imap.gmail.com"
	DefaultIMAPPort = 993
	DefaultSMTPHost = "smtp.gmail.com"
	DefaultSMTPPort = 587
)

// WithDefaults fills empty fields with the Gmail endpoints.
func (h MailHosts) WithDefaults() MailHosts {
	if h.IMAPHost == "" {
		h.IMAPHost = DefaultIMAPHost
	}
	if h.IMAPPort == 0 {
		h.IMAPPort = DefaultIMAPPort
	}
	if h.SMTPHost == "" {
		h.SMTPHost = DefaultSMTPHost
	}
	if h.SMTPPort == 0 {
		h.SMTPPort = DefaultSMTPPort
	}
	return h
}

// IMAPAddr and SMTPAddr render dial targets.
func (h MailHosts) IMAPAddr() string {
	d := h.WithDefaults()
	return d.IMAPHost + ":" + strconv.Itoa(d.IMAPPort)
}

func (h MailHosts) SMTPAddr() string {
	d := h.WithDefaults()
	return d.SMTPHost + ":" + strconv.Itoa(d.SMTPPort)
}
