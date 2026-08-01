package google

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
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
// Above it the source fetches the headers AND the text parts, and skips only the
// binary attachments, marking the capture Truncated and listing what it left
// behind. The body survives because a text part is a few KB even in a 12 MB
// message — an earlier design took headers only and threw away the ask along
// with the attachment. The point of the cap is that one 40 MiB attachment cannot
// stall a pass or bloat raw_source_items; the mailboxes in scope hold ~117k
// messages between them.
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
	// Truncated marks a partial capture: the message exceeded the size cap, so
	// its headers and text parts were fetched and its attachments were not.
	Truncated bool
	// Parts lists the non-text parts left behind on a truncated capture, so a
	// worker can still see that a contract was attached even when the bytes were
	// not worth storing.
	Parts []MessagePart
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

// ---- the real IMAP client ------------------------------------------------------
//
// Deliberately in this file: a test greps imap.go to prove the connector holds no
// verb that could change a mailbox, and that guarantee is worth nothing if the
// actual client lives somewhere the grep does not reach.
//
// Two rules the implementation below never breaks:
//
//  1. The mailbox is always SELECTed read-only, and every body fetch is
//     BODY.PEEK — belt and braces, because either one alone would keep the
//     server from marking mail as read.
//  2. Nothing is ever transferred that we do not intend to store. An oversize
//     message is fetched part-selectively (headers plus its text parts) rather
//     than whole-then-discarded.

// MessagePart describes one non-text MIME part we chose NOT to store.
//
// It exists because "this message had Q3-contract.pdf" is the context a worker
// needs even when the bytes are not worth putting in JSONB. Without it a
// truncated row cannot say whether it dropped a contract or a holiday photo.
type MessagePart struct {
	// PartID is the IMAP part path, e.g. "2" or "1.3" — enough to fetch this
	// exact part later without guessing.
	PartID      string `json:"part_id"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// IMAPClientSource is the production MailSource, speaking IMAP over TLS.
type IMAPClientSource struct {
	Host     string
	Port     int
	Username string
	// Password is the app password. Held only for the life of a pass, never
	// logged, and never included in an error string.
	Password string
	// TLSConfig overrides the default; production leaves it nil.
	TLSConfig *tls.Config

	mu   sync.Mutex
	conn *client.Client
	// selected records the last mailbox opened, for diagnostics. It is NOT a
	// re-SELECT cache: selectFolder always issues SELECT, because UIDVALIDITY is
	// read from its response and a stale value is how a cursor silently points at
	// the wrong generation of a folder.
	selected string
}

// NewIMAPClientSource builds a source for one mailbox.
func NewIMAPClientSource(hosts MailHosts, username, password string) *IMAPClientSource {
	h := hosts.WithDefaults()
	return &IMAPClientSource{Host: h.IMAPHost, Port: h.IMAPPort, Username: username, Password: password}
}

func (s *IMAPClientSource) addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// connect dials and authenticates, reusing a live connection.
//
// Implicit TLS only: Gmail's IMAP supports nothing else, and accepting a
// plaintext fallback would put an app password on the wire in clear.
func (s *IMAPClientSource) connect(ctx context.Context) (*client.Client, error) {
	if s.conn != nil {
		// Noop is the cheapest liveness probe; a dead connection fails here
		// rather than halfway through a fetch.
		if err := s.conn.Noop(); err == nil {
			return s.conn, nil
		}
		_ = s.conn.Logout()
		s.conn, s.selected = nil, ""
	}

	tlsCfg := s.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: s.Host}
	}
	conn, err := client.DialTLS(s.addr(), tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", s.addr(), err)
	}
	if err := conn.Login(s.Username, s.Password); err != nil {
		_ = conn.Logout()
		// The password is NOT interpolated: this string reaches logs and
		// sync_runs.stats.
		return nil, fmt.Errorf("imap login as %s failed: %w", s.Username, err)
	}
	s.conn = conn
	return conn, nil
}

// selectFolder opens a mailbox READ-ONLY. Read-only is the first of the two
// guarantees that a pass cannot alter message state.
func (s *IMAPClientSource) selectFolder(ctx context.Context, folder string) (*imap.MailboxStatus, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	status, err := conn.Select(folder, true /* read-only */)
	if err != nil {
		return nil, fmt.Errorf("imap select %s: %w", folder, err)
	}
	s.selected = folder
	return status, nil
}

// Close releases the connection. Callers that finish a pass should call it; the
// process exiting is also fine.
func (s *IMAPClientSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Logout()
	s.conn, s.selected = nil, ""
	return err
}

// Folders lists selectable mailboxes, carrying the RFC 6154 \Sent marker.
func (s *IMAPClientSource) Folders(ctx context.Context) ([]Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}

	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() { done <- conn.List("", "*", ch) }()

	var infos []*imap.MailboxInfo
	for m := range ch {
		infos = append(infos, m)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("imap list mailboxes: %w", err)
	}

	var out []Folder
	for _, m := range infos {
		f := Folder{Name: m.Name}
		for _, attr := range m.Attributes {
			switch strings.ToLower(attr) {
			case strings.ToLower(imap.SentAttr):
				f.Sent = true
			case strings.ToLower(imap.NoSelectAttr):
				// Container nodes hold no messages; SELECTing one errors.
				f.Name = ""
			}
		}
		if f.Name == "" {
			continue
		}
		// UIDVALIDITY comes from SELECT, not LIST, so it is filled by the loop
		// below.
		out = append(out, f)
	}

	// Fill UIDVALIDITY for the folders we will actually ingest — and ONLY those.
	//
	// UIDVALIDITY comes from SELECT, not LIST, so it costs a round trip per
	// folder. Selecting every listed mailbox to get it is what wedged
	// sspataro@gmail.com: a 20-year account lists ~29 mailboxes including
	// [Gmail]/All Mail, and opening All Mail on 175k+ messages is slow enough
	// that the whole pass deadline was gone before a single message was fetched.
	// The two small mailboxes never showed it.
	//
	// SelectFolders needs only the name and the \Sent marker, both of which LIST
	// already gave us, so the filter can run first and the round trips drop from
	// ~29 to 2. Applying it here and again in IngestIMAP is harmless: selecting
	// from an already-selected set returns the same set.
	wanted := SelectFolders(out, FoldersFromEnv())
	kept := make([]Folder, 0, len(wanted))
	for _, f := range wanted {
		status, err := s.selectFolder(ctx, f.Name)
		if err != nil {
			// A mailbox we cannot open is not fatal to the pass — it may be a
			// shared folder we lack rights on. Drop it rather than failing.
			continue
		}
		f.UIDValidity = status.UidValidity
		kept = append(kept, f)
	}
	return kept, nil
}

// Search returns matching UIDs ascending. Exactly one of the criteria bounds the
// scan; an unbounded search over a six-figure mailbox is what criterion 7 forbids.
func (s *IMAPClientSource) Search(ctx context.Context, folder string, crit SearchCriteria) ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.selectFolder(ctx, folder); err != nil {
		return nil, err
	}
	conn := s.conn

	if crit.FromUID != 0 {
		// UID range: {from}:* — the incremental pass.
		seq := new(imap.SeqSet)
		seq.AddRange(crit.FromUID, 0) // 0 == '*'
		criteria := imap.NewSearchCriteria()
		criteria.Uid = seq
		uids, err := conn.UidSearch(criteria)
		if err != nil {
			return nil, fmt.Errorf("imap uid search %s from %d: %w", folder, crit.FromUID, err)
		}
		return uids, nil
	}

	criteria := imap.NewSearchCriteria()
	if !crit.Since.IsZero() {
		criteria.Since = crit.Since
	}
	uids, err := conn.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("imap search %s since %s: %w", folder, crit.Since, err)
	}
	return uids, nil
}

// Fetch retrieves messages, capping transfer rather than truncating after the
// fact.
//
// Two passes on purpose. The first asks only for metadata and the MIME tree,
// which is cheap. Only then does it decide, per message, whether to pull the
// whole thing or just the headers and text parts — so a 40 MiB attachment is
// never put on the wire at all.
func (s *IMAPClientSource) Fetch(ctx context.Context, folder string, uids []uint32, maxBytes int) ([]FetchedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.selectFolder(ctx, folder); err != nil {
		return nil, err
	}
	conn := s.conn

	seq := new(imap.SeqSet)
	for _, u := range uids {
		seq.AddNum(u)
	}

	meta := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- conn.UidFetch(seq, []imap.FetchItem{
			imap.FetchUid, imap.FetchInternalDate, imap.FetchFlags,
			imap.FetchRFC822Size, imap.FetchBodyStructure,
		}, meta)
	}()
	var metas []*imap.Message
	for m := range meta {
		metas = append(metas, m)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("imap fetch metadata %s: %w", folder, err)
	}

	// Bodies for the whole batch in ONE round trip.
	//
	// This used to be one UID FETCH per message, which is what wedged
	// sspataro@gmail.com in production: thousands of sequential round trips
	// against a 106,930-message mailbox could not finish inside the pass deadline,
	// so the cursor never advanced and every run re-did the same work from
	// scratch — a livelock that made partial progress and never converged.
	// Oversize messages still need individual part-selective fetches, so they are
	// separated out and handled one at a time; there are very few of them.
	whole := make([]uint32, 0, len(metas))
	for _, m := range metas {
		if !(maxBytes > 0 && int(m.Size) > maxBytes) {
			whole = append(whole, m.Uid)
		}
	}
	bodies, err := s.fetchBodies(conn, whole)
	if err != nil {
		return nil, fmt.Errorf("imap fetch bodies %s: %w", folder, err)
	}

	var out []FetchedMessage
	for _, m := range metas {
		fm := FetchedMessage{
			UID:          m.Uid,
			InternalDate: m.InternalDate,
			Flags:        append([]string(nil), m.Flags...),
			Size:         int(m.Size),
		}
		if body, ok := bodies[m.Uid]; ok {
			fm.RFC822 = body
			out = append(out, fm)
			continue
		}

		// Oversize: keep the headers and the text, list what was left behind.
		textPath, textType, textEnc, parts := planOversizeFetch(m.BodyStructure)
		body, err := s.fetchHeadersAndText(conn, m.Uid, textPath, textType, textEnc)
		if err != nil {
			return nil, fmt.Errorf("imap fetch oversize %s/%d: %w", folder, m.Uid, err)
		}
		fm.RFC822 = body
		fm.Truncated = true
		fm.Parts = parts
		out = append(out, fm)
	}
	return out, nil
}

// fetchBodies pulls full bodies for many UIDs in a single UID FETCH.
func (s *IMAPClientSource) fetchBodies(conn *client.Client, uids []uint32) (map[uint32][]byte, error) {
	out := map[uint32][]byte{}
	if len(uids) == 0 {
		return out, nil
	}
	seq := new(imap.SeqSet)
	for _, u := range uids {
		seq.AddNum(u)
	}
	section := &imap.BodySectionName{Peek: true}
	ch := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- conn.UidFetch(seq, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, ch)
	}()
	for m := range ch {
		r := m.GetBody(section)
		if r == nil {
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read body for uid %d: %w", m.Uid, err)
		}
		out[m.Uid] = b
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return out, nil
}

// fetchWhole pulls the entire message with PEEK.
func (s *IMAPClientSource) fetchWhole(conn *client.Client, uid uint32) ([]byte, error) {
	section := &imap.BodySectionName{Peek: true}
	return fetchSection(conn, uid, section)
}

// fetchHeadersAndText builds a synthetic single-part message: the original
// headers with their content-type replaced by the text part's, followed by that
// part's bytes.
//
// Rewriting the content-type is not cosmetic. The original header says
// multipart/... with a boundary that is no longer present, and a parser handed
// that would find no body at all — which is exactly the bug this whole change
// exists to fix.
func (s *IMAPClientSource) fetchHeadersAndText(conn *client.Client, uid uint32, textPath []int, textType, textEnc string) ([]byte, error) {
	headerSection := &imap.BodySectionName{
		Peek:         true,
		BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier},
	}
	headers, err := fetchSection(conn, uid, headerSection)
	if err != nil {
		return nil, fmt.Errorf("fetch headers: %w", err)
	}
	if len(textPath) == 0 {
		// No text part at all (a bare attachment). Headers alone still carry
		// Message-ID, direction, subject and threading.
		return headers, nil
	}

	textSection := &imap.BodySectionName{
		Peek:         true,
		BodyPartName: imap.BodyPartName{Path: textPath},
	}
	text, err := fetchSection(conn, uid, textSection)
	if err != nil {
		return nil, fmt.Errorf("fetch text part %v: %w", textPath, err)
	}

	var b bytes.Buffer
	for _, line := range strings.Split(string(headers), "\r\n") {
		lower := strings.ToLower(line)
		// Drop the multipart framing headers; everything else is preserved
		// verbatim so identity, threading and dates survive untouched.
		if strings.HasPrefix(lower, "content-type:") || strings.HasPrefix(lower, "content-transfer-encoding:") {
			continue
		}
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	if textType == "" {
		textType = "text/plain"
	}
	fmt.Fprintf(&b, "Content-Type: %s\r\n", textType)
	if textEnc != "" {
		fmt.Fprintf(&b, "Content-Transfer-Encoding: %s\r\n", textEnc)
	}
	b.WriteString("\r\n")
	b.Write(text)
	return b.Bytes(), nil
}

// fetchSection runs one UID FETCH for a single body section.
func fetchSection(conn *client.Client, uid uint32, section *imap.BodySectionName) ([]byte, error) {
	seq := new(imap.SeqSet)
	seq.AddNum(uid)
	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() { done <- conn.UidFetch(seq, []imap.FetchItem{section.FetchItem()}, ch) }()

	var out []byte
	for m := range ch {
		r := m.GetBody(section)
		if r == nil {
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read body section: %w", err)
		}
		out = b
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return out, nil
}

// planOversizeFetch walks the MIME tree and decides what to pull.
//
// Returns the path of the first text/plain part (falling back to text/html),
// its type and transfer encoding, plus a manifest of every part left behind.
func planOversizeFetch(bs *imap.BodyStructure) (textPath []int, textType, textEnc string, parts []MessagePart) {
	if bs == nil {
		return nil, "", "", nil
	}
	var htmlPath []int
	var htmlType, htmlEnc string

	var walk func(node *imap.BodyStructure, path []int)
	walk = func(node *imap.BodyStructure, path []int) {
		if node == nil {
			return
		}
		if len(node.Parts) > 0 {
			for i, child := range node.Parts {
				walk(child, append(append([]int(nil), path...), i+1))
			}
			return
		}
		mediaType := strings.ToLower(node.MIMEType + "/" + node.MIMESubType)
		switch {
		case mediaType == "text/plain" && textPath == nil:
			textPath = path
			textType = mediaType + charsetParam(node)
			textEnc = node.Encoding
		case mediaType == "text/html" && htmlPath == nil:
			htmlPath = path
			htmlType = mediaType + charsetParam(node)
			htmlEnc = node.Encoding
		default:
			parts = append(parts, MessagePart{
				PartID:      pathString(path),
				Filename:    partFilename(node),
				ContentType: mediaType,
				Size:        int(node.Size),
			})
		}
	}
	walk(bs, nil)

	if textPath == nil && htmlPath != nil {
		return htmlPath, htmlType, htmlEnc, parts
	}
	return textPath, textType, textEnc, parts
}

func charsetParam(node *imap.BodyStructure) string {
	if cs := node.Params["charset"]; cs != "" {
		return `; charset="` + cs + `"`
	}
	return ""
}

// partFilename reads the name from Content-Disposition, falling back to the
// Content-Type name parameter — senders use both.
func partFilename(node *imap.BodyStructure) string {
	if node.DispositionParams != nil {
		if n := node.DispositionParams["filename"]; n != "" {
			return n
		}
	}
	if node.Params != nil {
		if n := node.Params["name"]; n != "" {
			return n
		}
	}
	return ""
}

func pathString(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	out := make([]string, len(path))
	for i, p := range path {
		out[i] = strconv.Itoa(p)
	}
	return strings.Join(out, ".")
}

// Idle blocks until the folder reports a change, then signals once.
//
// The signal is a WAKE-UP, never a payload: the caller re-runs a bounded UID
// fetch, so a missed or duplicated notification costs a round trip and never a
// lost message. Same discipline the orchestrator applies to Postgres NOTIFY.
func (s *IMAPClientSource) Idle(ctx context.Context, folder string) (<-chan struct{}, error) {
	s.mu.Lock()
	if _, err := s.selectFolder(ctx, folder); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	conn := s.conn
	s.mu.Unlock()

	out := make(chan struct{}, 1)
	updates := make(chan client.Update, 8)
	conn.Updates = updates

	stop := make(chan struct{})
	idleDone := make(chan error, 1)
	go func() {
		// IDLE is re-issued by the caller; RFC 2177 requires a refresh at least
		// every 29 minutes, and MAIL_IDLE_REFRESH keeps us inside that.
		idleDone <- conn.Idle(stop, nil)
	}()

	go func() {
		defer close(out)
		defer func() {
			close(stop)
			<-idleDone
			s.mu.Lock()
			if s.conn == conn {
				conn.Updates = nil
			}
			s.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case u, ok := <-updates:
				if !ok {
					return
				}
				// Only mailbox updates matter; status and removal chatter is noise.
				if _, isMailbox := u.(*client.MailboxUpdate); !isMailbox {
					continue
				}
				select {
				case out <- struct{}{}:
				default: // a pending wake-up already covers this change
				}
				return
			}
		}
	}()
	return out, nil
}
