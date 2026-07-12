package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/sspataro57/switchboard/internal/executor"
)

// RawStore stores webhook deliveries raw-first (external_id delivery:{guid}
// under the synthetic provider='github' account).
type RawStore interface {
	StoreDelivery(ctx context.Context, guid, eventType string, body []byte) (already bool, err error)
}

// TaskResolver maps a PRRef to a task: newest active external_ref, or the
// task-{N}-* head-branch fallback. ok=false means not ours.
type TaskResolver interface {
	Resolve(ctx context.Context, ref PRRef) (taskID int64, ok bool, err error)
}

// Dispatcher is the executor seam.
type Dispatcher interface {
	Execute(ctx context.Context, call executor.Call) (executor.Result, error)
}

// Actor is the receiver's executor identity.
const Actor = "hooksd:github"

type receiver struct {
	secret   string
	store    RawStore
	resolver TaskResolver
	ex       Dispatcher
}

// NewReceiver builds the webhook http.Handler: HMAC verify → raw store →
// resolve → dispatch.
func NewReceiver(secret string, store RawStore, resolver TaskResolver, ex Dispatcher) http.Handler {
	return &receiver{secret: secret, store: store, resolver: resolver, ex: ex}
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !verifySignature(rc.secret, r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	guid := r.Header.Get("X-GitHub-Delivery")
	ctx := r.Context()

	// Raw-first (invariant 1): the delivery lands before any interpretation.
	already, err := rc.store.StoreDelivery(ctx, guid, eventType, body)
	if err != nil {
		slog.Error("store webhook delivery", "guid", guid, "err", err)
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	if already {
		w.WriteHeader(http.StatusNoContent) // redelivery dedup
		return
	}

	intents, err := MapEvent(eventType, body)
	if err != nil {
		slog.Warn("unmappable webhook payload", "event", eventType, "guid", guid, "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	for _, in := range intents {
		taskID, ok, err := rc.resolver.Resolve(ctx, in.Match)
		if err != nil {
			slog.Error("resolve PR to task", "match", in.Match, "err", err)
			continue
		}
		if !ok {
			continue // not ours: stored raw, no events
		}
		args := make(map[string]any, len(in.Args)+1)
		for k, v := range in.Args {
			args[k] = v
		}
		args["task_id"] = taskID
		raw, err := json.Marshal(args)
		if err != nil {
			slog.Error("marshal intent args", "err", err)
			continue
		}
		if _, err := rc.ex.Execute(ctx, executor.Call{Tool: in.Tool, Actor: Actor, Args: raw}); err != nil {
			slog.Error("dispatch webhook intent", "tool", in.Tool, "task", taskID, "err", err)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func verifySignature(secret, header string, body []byte) bool {
	if header == "" || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(header), []byte(want))
}
