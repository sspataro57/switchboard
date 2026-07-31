package google_test

// Shared OFFLINE fake of the IMAP surface for the SWT-11 unit tests
// (rfc822_test.go, imap_ingest_test.go) AND the compose-db integration suite
// (imap_integration_test.go). NO build tag: it compiles into both the default
// `go test` binary and the `-tags integration` binary, so the fake is defined
// once — same shape as fake_google_test.go.
//
// NEVER a live IMAP connection (SPEC "Invariants that apply", 7: all I/O lives
// behind the MailSource interface, so the ingest phase is unit-testable with
// zero network and zero Postgres).
//
// GREENFIELD NOTE: internal/connector/google gains imap.go / imap_ingest.go /
// rfc822.go this ticket; every file that imports these symbols compile-FAILs
// under `go test ./...` until they exist — the expected failure mode. For
// greenfield code the SPEC's contract IS the signature. Imposed exported
// surface (SPEC "Files likely to touch" → new imap.go, criteria 5-10):
//
//   // MAIL_MAX_MESSAGE_BYTES default (criterion 7): above it a message is
//   // captured headers-only with "truncated": true.
//   const DefaultMaxMessageBytes = 1 << 20
//   // Fallback Sent folder when LIST exposes no \Sent SPECIAL-USE (criterion 9).
//   const DefaultSentFolder = "[Gmail]/Sent Mail"
//
//   // Folder is one mailbox from LIST with its RFC 6154 role + UIDVALIDITY.
//   type Folder struct {
//       Name        string
//       Sent        bool   // \Sent SPECIAL-USE attribute
//       UIDValidity uint32
//   }
//
//   // SearchCriteria bounds ONE folder pass — a 106,930-message mailbox must
//   // never produce a full-mailbox fetch (criterion 7). Exactly one of the two
//   // is set: Since = SEARCH SINCE (first pass / --full / UIDVALIDITY resync);
//   // FromUID = the incremental UID FETCH {uid_next}:* range (criterion 8).
//   type SearchCriteria struct {
//       Since   time.Time
//       FromUID uint32
//   }
//
//   // FetchedMessage is one message read with BODY.PEEK[...] (criterion 6 —
//   // there is no write verb against the mailbox anywhere in this interface).
//   type FetchedMessage struct {
//       UID          uint32
//       InternalDate time.Time
//       Flags        []string
//       Size         int
//       Truncated    bool   // headers-only capture above maxBytes
//       RFC822       []byte // 8-bit-clean bytes as received
//   }
//
//   type MailSource interface {
//       Folders(ctx context.Context) ([]Folder, error)
//       Search(ctx context.Context, folder string, crit SearchCriteria) ([]uint32, error)
//       Fetch(ctx context.Context, folder string, uids []uint32, maxBytes int) ([]FetchedMessage, error)
//       Idle(ctx context.Context, folder string) (<-chan struct{}, error) // watch mode only
//   }
//
//   // Per-folder cursor inside source_accounts.sync_cursor (criterion 8):
//   //   {"imap_folders": {"INBOX": {"uidvalidity": 12, "uid_next": 88231}}}
//   type FolderCursor struct {
//       UIDValidity uint32 `json:"uidvalidity"`
//       UIDNext     uint32 `json:"uid_next"`
//   }
//   // Cursor gains IMAPFolders map[string]FolderCursor `json:"imap_folders,omitempty"`
//   // Stats  gains IMAPListed / IMAPFetched / IMAPTruncated ints
//   // Config gains MaxMessageBytes int, Folders []string (MAIL_FOLDERS override)
//
//   func IngestIMAP(ctx context.Context, src MailSource, sink Sink, acct Account, cfg Config) (Stats, error)
//
//   // Pure normalizer over ONE raw_source_items.raw_json envelope (criterion 10).
//   func NormalizeRFC822(raw json.RawMessage, accountEmail string, ownEmails map[string]bool) (NormalizedMessage, error)
//
// If the implementation's Idle signature ends up differing, only this fake needs
// the edit — no test here ever calls it; it exists so the fake satisfies the
// SPEC's full MailSource method set.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	imapINBOX = "INBOX"
	imapSent  = "[Gmail]/Sent Mail"
)

// ---- raw envelope (criterion 5) ---------------------------------------------

// imapEnvelope mirrors raw_source_items.raw_json for an `imap:` raw item:
//
//	{"source":"imap","folder":...,"uidvalidity":N,"uid":M,
//	 "internaldate":"RFC3339","flags":[...],"size":N,"truncated":bool,
//	 "rfc822_b64":"<base64 of the bytes as received>"}
//
// Base64, not a JSON string: RFC822 is 8-bit-clean and would not survive UTF-8
// validation (SPEC decision 5).
type imapEnvelope struct {
	Source       string   `json:"source"`
	Folder       string   `json:"folder"`
	UIDValidity  uint32   `json:"uidvalidity"`
	UID          uint32   `json:"uid"`
	InternalDate string   `json:"internaldate"`
	Flags        []string `json:"flags"`
	Size         int      `json:"size"`
	Truncated    bool     `json:"truncated"`
	RFC822B64    string   `json:"rfc822_b64"`
}

// newIMAPEnvelope builds the raw_json the ingest phase is contracted to write —
// used as INPUT to NormalizeRFC822 (which reads only raw_json) and as the
// expected shape in the ingest tests.
func newIMAPEnvelope(folder string, uidValidity, uid uint32, internalDate time.Time, flags []string, rfc822 []byte, truncated bool) json.RawMessage {
	if flags == nil {
		flags = []string{}
	}
	env := imapEnvelope{
		Source:       "imap",
		Folder:       folder,
		UIDValidity:  uidValidity,
		UID:          uid,
		InternalDate: internalDate.UTC().Format(time.RFC3339),
		Flags:        flags,
		Size:         len(rfc822),
		Truncated:    truncated,
		RFC822B64:    base64.StdEncoding.EncodeToString(rfc822),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return raw
}

// decodeB64 accepts padded or unpadded standard base64. The contract that
// matters is losslessness (SPEC decision 5), not the padding dialect.
func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("rfc822_b64 is not base64: %v", err)
	}
	return b
}

// rfc822 assembles a CRLF message from header lines + body (the wire form the
// IMAP server hands back).
func rfc822(headers []string, body string) []byte {
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)
}

// rfc822Headers assembles a headers-only capture (the >MAIL_MAX_MESSAGE_BYTES
// truncation form, criterion 7).
func rfc822Headers(headers []string) []byte {
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n")
}

// ---- fake MailSource ---------------------------------------------------------

type fakeSearchCall struct {
	folder string
	crit   google.SearchCriteria
}

type fakeFetchCall struct {
	folder   string
	uids     []uint32
	maxBytes int
}

// fakeIMAP is an in-memory MailSource. It has NO write verb — there is nothing
// in this fake that could set \Seen, move, delete or label, because the
// interface it implements has no such method (criterion 6).
type fakeIMAP struct {
	folders []google.Folder
	msgs    map[string][]google.FetchedMessage // folder -> messages, UID ascending

	searches []fakeSearchCall
	fetches  []fakeFetchCall

	foldersErr  error
	searchErr   error
	fetchErrUID uint32 // when non-zero, Fetch fails if this UID is requested
	unreachable bool   // every call errors (the --normalize-only --all case)
}

func newFakeIMAP() *fakeIMAP {
	return &fakeIMAP{msgs: map[string][]google.FetchedMessage{}}
}

// add appends one message to a folder (UID order is the caller's).
func (f *fakeIMAP) add(folder string, m google.FetchedMessage) {
	f.msgs[folder] = append(f.msgs[folder], m)
	sort.Slice(f.msgs[folder], func(i, j int) bool { return f.msgs[folder][i].UID < f.msgs[folder][j].UID })
}

func (f *fakeIMAP) Folders(context.Context) ([]google.Folder, error) {
	if f.unreachable {
		return nil, fmt.Errorf("imap unreachable (fake)")
	}
	if f.foldersErr != nil {
		return nil, f.foldersErr
	}
	return f.folders, nil
}

func (f *fakeIMAP) Search(_ context.Context, folder string, crit google.SearchCriteria) ([]uint32, error) {
	if f.unreachable {
		return nil, fmt.Errorf("imap unreachable (fake)")
	}
	f.searches = append(f.searches, fakeSearchCall{folder: folder, crit: crit})
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	var out []uint32
	for _, m := range f.msgs[folder] {
		if !crit.Since.IsZero() && m.InternalDate.Before(crit.Since) {
			continue
		}
		if crit.FromUID != 0 && m.UID < crit.FromUID {
			continue
		}
		out = append(out, m.UID)
	}
	return out, nil
}

func (f *fakeIMAP) Fetch(_ context.Context, folder string, uids []uint32, maxBytes int) ([]google.FetchedMessage, error) {
	if f.unreachable {
		return nil, fmt.Errorf("imap unreachable (fake)")
	}
	f.fetches = append(f.fetches, fakeFetchCall{folder: folder, uids: append([]uint32(nil), uids...), maxBytes: maxBytes})
	want := map[uint32]bool{}
	for _, u := range uids {
		if f.fetchErrUID != 0 && u == f.fetchErrUID {
			return nil, fmt.Errorf("fetch uid %d: fake IMAP failure", u)
		}
		want[u] = true
	}
	var out []google.FetchedMessage
	for _, m := range f.msgs[folder] {
		if !want[m.UID] {
			continue
		}
		// The size cap lives in the source (headers-only above maxBytes,
		// criterion 7); the phase only records what it was handed.
		if maxBytes > 0 && m.Size > maxBytes && !m.Truncated {
			m.Truncated = true
			m.RFC822 = headersOf(m.RFC822)
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeIMAP) Idle(context.Context, string) (<-chan struct{}, error) {
	// Watch mode only; never exercised by these tests.
	ch := make(chan struct{})
	return ch, nil
}

// headersOf keeps everything up to and including the header/body separator.
func headersOf(msg []byte) []byte {
	if i := strings.Index(string(msg), "\r\n\r\n"); i >= 0 {
		return msg[:i+4]
	}
	return msg
}

// ---- fake Sink that keeps the raw bytes -------------------------------------

// imapRawWrite is one recorded raw_source_items write.
type imapRawWrite struct {
	externalID string
	raw        json.RawMessage
	hash       string
	update     bool
}

// fakeIMAPSink implements google.Sink and, unlike poller_test.go's fakeSink,
// RETAINS raw_json so the envelope contract (criterion 5) can be asserted.
type fakeIMAPSink struct {
	cursor       google.Cursor
	stored       map[string]string // externalID -> content_hash
	writes       []imapRawWrite
	savedCursors []google.Cursor
	runs         []string
	finishStatus string
	finishStats  google.Stats
	saveCursorAt []int // len(writes) at each SaveCursor (cursor-advanced-last proof)
}

func newFakeIMAPSink() *fakeIMAPSink {
	return &fakeIMAPSink{stored: map[string]string{}}
}

func (s *fakeIMAPSink) Cursor(context.Context, int64) (google.Cursor, error) { return s.cursor, nil }

func (s *fakeIMAPSink) SaveCursor(_ context.Context, _ int64, c google.Cursor) error {
	s.savedCursors = append(s.savedCursors, c)
	s.saveCursorAt = append(s.saveCursorAt, len(s.writes))
	s.cursor = c
	return nil
}

func (s *fakeIMAPSink) StartRun(_ context.Context, _ int64, phase string) (int64, error) {
	s.runs = append(s.runs, "start:"+phase)
	return int64(len(s.runs)), nil
}

func (s *fakeIMAPSink) FinishRun(_ context.Context, _ int64, status string, stats google.Stats, _ string) error {
	s.runs = append(s.runs, "finish:"+status)
	s.finishStatus = status
	s.finishStats = stats
	return nil
}

func (s *fakeIMAPSink) RawHash(_ context.Context, _ int64, externalID string) (string, bool, error) {
	h, ok := s.stored[externalID]
	return h, ok, nil
}

func (s *fakeIMAPSink) InsertRaw(_ context.Context, _ int64, externalID string, raw json.RawMessage, hash string) error {
	s.writes = append(s.writes, imapRawWrite{externalID: externalID, raw: append(json.RawMessage(nil), raw...), hash: hash})
	s.stored[externalID] = hash
	return nil
}

func (s *fakeIMAPSink) UpdateRaw(_ context.Context, _ int64, externalID string, raw json.RawMessage, hash string) error {
	s.writes = append(s.writes, imapRawWrite{externalID: externalID, raw: append(json.RawMessage(nil), raw...), hash: hash, update: true})
	s.stored[externalID] = hash
	return nil
}

// write returns the recorded write for an external id, or nil.
func (s *fakeIMAPSink) write(externalID string) *imapRawWrite {
	for i := range s.writes {
		if s.writes[i].externalID == externalID {
			return &s.writes[i]
		}
	}
	return nil
}

// envelope decodes a recorded raw_json into the envelope struct.
func (s *fakeIMAPSink) envelope(t *testing.T, externalID string) imapEnvelope {
	t.Helper()
	w := s.write(externalID)
	if w == nil {
		var ids []string
		for _, x := range s.writes {
			ids = append(ids, x.externalID)
		}
		t.Fatalf("no raw row written for %q; wrote %v", externalID, ids)
	}
	var env imapEnvelope
	if err := json.Unmarshal(w.raw, &env); err != nil {
		t.Fatalf("raw_json for %q is not the imap envelope: %v (%s)", externalID, err, w.raw)
	}
	return env
}

// imapAccount is the account under test in the offline suites.
func imapAccount() google.Account {
	return google.Account{ID: 77, Email: acctA, CalendarInAvailability: false}
}
