package upworkcrm_test

// Unit tests for the deterministic raw -> canonical normalization
// (SPEC 02-upwork-crm-connector, acceptance criterion 4; invariant 7 discipline
// transfer: normalize is a PURE function of the raw row — zero network, no LLM).
// Input is a raw_source_items.raw_json row (verbatim source column names,
// snake_case) exactly as criterion 7 (re-normalize from raw alone) requires.
//
// GREENFIELD NOTE: package internal/connector/upworkcrm does not exist yet;
// this file compile-FAILs under `go test ./...` until it is implemented. That
// is the expected failure mode. Expected exported surface (the SPEC's
// normalize.go):
//
//   type Identity struct { Provider, Value string } // provider: upwork_crm|email|upwork_room
//
//   type NormalizedClient struct {
//       ClientID    string     // source clients.id (uuid)
//       DisplayName string     // from clients.name
//       Identities  []Identity // upwork_crm:{uuid} always; email/upwork_room when present
//   }
//
//   type NormalizedMessage struct {
//       ThreadKey         string    // upwork_crm:{client uuid}:{channel}
//       ClientID          string    // source communications.client_id (participant ref)
//       Direction         string    // inbound|outbound
//       SentAt            time.Time // = communications.communicated_at
//       BodyText          string
//       Subject           string
//       Sender            string
//       Channel           string
//       ExternalMessageID string    // = communications.external_id, VERBATIM (invariant 5)
//   }
//
//   // Pure mappers over one raw row.
//   func NormalizeClient(raw json.RawMessage) (NormalizedClient, error)
//   func NormalizeCommunication(raw json.RawMessage) (NormalizedMessage, error)
//
//   // Identity resolution with NO auto-merge. Given the current owner of each
//   // (provider,value), decide which secondary identities to insert and which
//   // are suspected merges (owned by a DIFFERENT person). Pure given a resolver.
//   type IdentityResolver interface {
//       OwnerOf(ctx context.Context, provider, value string) (personID int64, ok bool, err error)
//   }
//   func ReconcileIdentities(ctx context.Context, personID int64, ids []Identity, r IdentityResolver) (insert, suspected []Identity, err error)

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

const (
	clientUUID = "11111111-1111-1111-1111-111111111111"
	commUUID   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func containsIdentity(ids []upworkcrm.Identity, provider, value string) bool {
	for _, id := range ids {
		if id.Provider == provider && id.Value == value {
			return true
		}
	}
	return false
}

// Client -> people + identities: upwork_crm:{uuid} ALWAYS; email:{...} and
// upwork_room:{...} only when those columns are present/non-null.
func TestNormalizeClient_Identities(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "` + clientUUID + `",
		"name": "Acme Corp",
		"email": "ops@acme.example",
		"company": "Acme",
		"upwork_room_id": "room-777",
		"created_at": "2026-01-02T03:04:05Z",
		"updated_at": "2026-01-02T03:04:05Z"
	}`)

	nc, err := upworkcrm.NormalizeClient(raw)
	if err != nil {
		t.Fatalf("NormalizeClient: %v", err)
	}
	if nc.DisplayName != "Acme Corp" {
		t.Errorf("DisplayName = %q, want %q", nc.DisplayName, "Acme Corp")
	}
	if nc.ClientID != clientUUID {
		t.Errorf("ClientID = %q, want %q", nc.ClientID, clientUUID)
	}
	if !containsIdentity(nc.Identities, "upwork_crm", clientUUID) {
		t.Errorf("missing always-present upwork_crm identity in %+v", nc.Identities)
	}
	if !containsIdentity(nc.Identities, "email", "ops@acme.example") {
		t.Errorf("missing email identity in %+v", nc.Identities)
	}
	if !containsIdentity(nc.Identities, "upwork_room", "room-777") {
		t.Errorf("missing upwork_room identity in %+v", nc.Identities)
	}
}

// A client with no email and no room yields ONLY the upwork_crm identity —
// no empty-valued email/upwork_room rows.
func TestNormalizeClient_MinimalIdentitiesWhenAbsent(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "` + clientUUID + `",
		"name": "Solo",
		"email": null,
		"upwork_room_id": null
	}`)

	nc, err := upworkcrm.NormalizeClient(raw)
	if err != nil {
		t.Fatalf("NormalizeClient: %v", err)
	}
	if !containsIdentity(nc.Identities, "upwork_crm", clientUUID) {
		t.Errorf("missing always-present upwork_crm identity in %+v", nc.Identities)
	}
	for _, id := range nc.Identities {
		if id.Provider == "email" || id.Provider == "upwork_room" {
			t.Errorf("unexpected identity for absent field: %+v", id)
		}
		if id.Value == "" {
			t.Errorf("empty-valued identity emitted: %+v", id)
		}
	}
}

// Communication -> message: field mapping, sent_at = communicated_at, thread key
// = upwork_crm:{client uuid}:{channel}, external_message_id preserved verbatim.
func TestNormalizeCommunication_MessageFields(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "` + commUUID + `",
		"client_id": "` + clientUUID + `",
		"direction": "inbound",
		"channel": "upwork",
		"subject": "Re: milestone",
		"body": "looks good, ship it",
		"communicated_at": "2026-07-01T10:00:00Z",
		"created_at": "2026-07-01T10:00:05Z",
		"sender": "client@acme.example",
		"external_id": "upwork-msg-42",
		"is_draft": false
	}`)

	nm, err := upworkcrm.NormalizeCommunication(raw)
	if err != nil {
		t.Fatalf("NormalizeCommunication: %v", err)
	}

	wantSentAt, _ := time.Parse(time.RFC3339, "2026-07-01T10:00:00Z")
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ThreadKey", nm.ThreadKey, "upwork_crm:" + clientUUID + ":upwork"},
		{"ClientID", nm.ClientID, clientUUID},
		{"Direction", nm.Direction, "inbound"},
		{"SentAt", nm.SentAt.UTC(), wantSentAt.UTC()},
		{"BodyText", nm.BodyText, "looks good, ship it"},
		{"Subject", nm.Subject, "Re: milestone"},
		{"Sender", nm.Sender, "client@acme.example"},
		{"Channel", nm.Channel, "upwork"},
		{"ExternalMessageID", nm.ExternalMessageID, "upwork-msg-42"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// Determinism (invariant 7): the same raw row normalizes to identical output
// on repeated calls.
func TestNormalizeCommunication_Deterministic(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "` + commUUID + `",
		"client_id": "` + clientUUID + `",
		"direction": "outbound",
		"channel": "email",
		"subject": "status",
		"body": "on track",
		"communicated_at": "2026-07-02T12:00:00Z",
		"created_at": "2026-07-02T12:00:01Z",
		"sender": "me@switchboard.example",
		"external_id": "gmail-abc",
		"is_draft": false
	}`)

	a, err := upworkcrm.NormalizeCommunication(raw)
	if err != nil {
		t.Fatalf("NormalizeCommunication (a): %v", err)
	}
	b, err := upworkcrm.NormalizeCommunication(raw)
	if err != nil {
		t.Fatalf("NormalizeCommunication (b): %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("normalization not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}

// fakeResolver reports the current owner of an identity value. personID 0 / ok
// false means unowned.
type fakeResolver struct {
	owners map[string]int64 // "provider|value" -> personID
}

func (f fakeResolver) OwnerOf(_ context.Context, provider, value string) (int64, bool, error) {
	id, ok := f.owners[provider+"|"+value]
	return id, ok, nil
}

// Identity conflict: a secondary identity already owned by a DIFFERENT person is
// counted as a suspected merge and NOT inserted (no auto-merge). One already
// owned by the SAME person is a no-op (neither inserted nor a merge). An unowned
// one is inserted.
func TestReconcileIdentities_SuspectedMergeNoAutoMerge(t *testing.T) {
	ctx := context.Background()
	const person = int64(10)

	ids := []upworkcrm.Identity{
		{Provider: "email", Value: "shared@acme.example"}, // owned by someone else -> suspected
		{Provider: "upwork_room", Value: "room-mine"},     // already owned by person 10 -> no-op
		{Provider: "email", Value: "fresh@acme.example"},  // unowned -> insert
	}
	r := fakeResolver{owners: map[string]int64{
		"email|shared@acme.example": 99,     // a different person
		"upwork_room|room-mine":     person, // us
	}}

	insert, suspected, err := upworkcrm.ReconcileIdentities(ctx, person, ids, r)
	if err != nil {
		t.Fatalf("ReconcileIdentities: %v", err)
	}

	if !containsIdentity(suspected, "email", "shared@acme.example") {
		t.Errorf("cross-owned identity not counted as suspected merge: %+v", suspected)
	}
	if len(suspected) != 1 {
		t.Errorf("suspected merges = %d (%+v), want exactly 1", len(suspected), suspected)
	}
	if containsIdentity(insert, "email", "shared@acme.example") {
		t.Errorf("suspected-merge identity must NOT be inserted (no auto-merge): %+v", insert)
	}
	if containsIdentity(insert, "upwork_room", "room-mine") {
		t.Errorf("identity already owned by same person must not be re-inserted: %+v", insert)
	}
	if !containsIdentity(insert, "email", "fresh@acme.example") {
		t.Errorf("unowned identity must be inserted: %+v", insert)
	}
	if len(insert) != 1 {
		t.Errorf("inserts = %d (%+v), want exactly 1", len(insert), insert)
	}
}
