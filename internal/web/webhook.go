package web

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"expvar"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// Expvar counters for webhook observability. All three are incremented on the
// hot path; they are surfaced via /debug/vars when MCP_EXPOSE_EXPVAR=1.
var (
	webhookReceivedTotal = expvar.NewInt("auth_webhook_received_total")
	webhookAcceptedTotal = expvar.NewInt("auth_webhook_accepted_total")
	webhookRejectedTotal = expvar.NewInt("auth_webhook_rejected_total")
)

// webhookPayload is the Supabase Database Webhook body shape.
// Unknown fields are ignored for forward-compat with Supabase schema changes.
type webhookPayload struct {
	Type      string         `json:"type"`
	Table     string         `json:"table"`
	Schema    string         `json:"schema"`
	Record    *webhookRecord `json:"record"`
	OldRecord *webhookRecord `json:"old_record"`
}

// webhookRecord holds the relevant fields from auth.users that the webhook
// handler inspects. Unknown fields from Supabase are silently ignored.
type webhookRecord struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	BannedUntil *string `json:"banned_until"`
	DeletedAt   *string `json:"deleted_at"`
}

// revocationUpdater is the interface the webhook handler needs to update the
// revoked_at column. Expressed as an interface to allow test injection without
// a live DB pool.
type revocationUpdater interface {
	SetUserRevoked(ctx context.Context, id string, revokedAt *time.Time) error
}

// poolRevocationUpdater wraps *pgxpool.Pool to satisfy revocationUpdater.
type poolRevocationUpdater struct {
	pool *pgxpool.Pool
}

func (p *poolRevocationUpdater) SetUserRevoked(ctx context.Context, id string, revokedAt *time.Time) error {
	return store.SetUserRevoked(ctx, p.pool, id, revokedAt)
}

// webhookHandler handles POST /auth/webhook.
// Authentication is HMAC-based (X-Webhook-Secret header), not Bearer JWT.
// It is registered BEFORE auth.Middleware so the route is reachable without
// a bearer token.
type webhookHandler struct {
	db              revocationUpdater
	revocationCache *auth.RevocationCache
	secret          string
}

// newWebhookHandler builds the webhook handler using the SUPABASE_WEBHOOK_SECRET
// env var. The secret is read once at construction time.
func newWebhookHandler(pool *pgxpool.Pool, cache *auth.RevocationCache) *webhookHandler {
	return &webhookHandler{
		db:              &poolRevocationUpdater{pool: pool},
		revocationCache: cache,
		secret:          os.Getenv("SUPABASE_WEBHOOK_SECRET"),
	}
}

// ServeHTTP processes a Supabase Database Webhook POST.
// Order: HMAC verify → parse body → dispatch on (type, schema, table).
func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	webhookReceivedTotal.Add(1)

	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.verifySecret(r) {
		webhookRejectedTotal.Add(1)
		auth.WriteError(w, auth.CodeWebhookInvalidSignature, "")
		return
	}

	payload, ok := h.parseBody(w, r)
	if !ok {
		// parseBody writes 200 for permanently malformed bodies to avoid
		// pg_net infinite retries. We do not increment accepted/rejected here.
		return
	}

	h.dispatch(r.Context(), w, payload)
	webhookAcceptedTotal.Add(1)
}

// verifySecret does a constant-time comparison of the X-Webhook-Secret header
// against the configured secret.
//
// Supabase bug #38848: Authorization headers are stripped from Database Webhooks
// by pg_net. We use a custom X-Webhook-Secret header instead.
func (h *webhookHandler) verifySecret(r *http.Request) bool {
	received := r.Header.Get("X-Webhook-Secret")
	// hmac.Equal is constant-time — prevents timing oracle attacks even though
	// the values are plain strings rather than HMAC digests.
	return hmac.Equal([]byte(received), []byte(h.secret))
}

// parseBody decodes the JSON body tolerantly. On malformed input it logs a
// warning and returns 200 to prevent pg_net from retrying on permanent garbage.
func (h *webhookHandler) parseBody(w http.ResponseWriter, r *http.Request) (*webhookPayload, bool) {
	var p webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		slog.Warn("webhook: malformed body", "error", err)
		// 200 to avoid pg_net infinite retries on a permanently malformed payload.
		w.WriteHeader(http.StatusOK)
		return nil, false
	}
	return &p, true
}

// dispatch routes the webhook payload to the appropriate action based on
// (type, schema, table). Irrelevant events receive a 200 no-op.
func (h *webhookHandler) dispatch(ctx context.Context, w http.ResponseWriter, p *webhookPayload) {
	if p.Schema != "auth" || p.Table != "users" {
		slog.Info("webhook ignored", "type", p.Type, "table", p.Table, "schema", p.Schema)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch p.Type {
	case "DELETE":
		h.handleDelete(ctx, w, p)
	case "UPDATE":
		h.handleUpdate(ctx, w, p)
	default:
		slog.Info("webhook ignored", "type", p.Type, "table", p.Table, "schema", p.Schema)
		w.WriteHeader(http.StatusOK)
	}
}

// handleDelete processes a DELETE event on auth.users.
// It revokes the user by setting revoked_at = now() and invalidates the cache.
func (h *webhookHandler) handleDelete(ctx context.Context, w http.ResponseWriter, p *webhookPayload) {
	rec := p.OldRecord
	if rec == nil || rec.ID == "" {
		slog.Warn("webhook: DELETE event missing old_record.id")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Always invalidate cache regardless of DB outcome — reduces revocation
	// latency even if the DB write fails transiently.
	defer h.revocationCache.Invalidate(rec.ID)

	now := time.Now()
	if err := h.db.SetUserRevoked(ctx, rec.ID, &now); err != nil {
		slog.Error("webhook: SetUserRevoked failed", "sub_prefix", subPrefix(rec.ID), "error", err)
	} else {
		slog.Info("webhook: user revoked (DELETE)", "sub_prefix", subPrefix(rec.ID))
	}

	w.WriteHeader(http.StatusOK)
}

// handleUpdate processes an UPDATE event on auth.users.
// It revokes when banned_until is a non-null future timestamp or deleted_at is
// set, and un-revokes when both are null.
func (h *webhookHandler) handleUpdate(ctx context.Context, w http.ResponseWriter, p *webhookPayload) {
	rec := p.Record
	if rec == nil || rec.ID == "" {
		slog.Warn("webhook: UPDATE event missing record.id")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Always invalidate cache regardless of DB outcome — reduces revocation
	// latency even if the DB write fails transiently.
	defer h.revocationCache.Invalidate(rec.ID)

	shouldRevoke := isFutureBannedUntil(rec.BannedUntil) || rec.DeletedAt != nil

	var revokedAt *time.Time
	if shouldRevoke {
		now := time.Now()
		revokedAt = &now
	}

	if err := h.db.SetUserRevoked(ctx, rec.ID, revokedAt); err != nil {
		slog.Error("webhook: SetUserRevoked failed", "sub_prefix", subPrefix(rec.ID), "error", err)
	} else {
		if shouldRevoke {
			slog.Info("webhook: user revoked (UPDATE)", "sub_prefix", subPrefix(rec.ID))
		} else {
			slog.Info("webhook: user un-revoked (UPDATE)", "sub_prefix", subPrefix(rec.ID))
		}
	}

	w.WriteHeader(http.StatusOK)
}

// isFutureBannedUntil returns true when bannedUntil is a non-null RFC3339
// timestamp that is strictly after the current time.
func isFutureBannedUntil(bannedUntil *string) bool {
	if bannedUntil == nil {
		return false
	}
	// Try RFC3339 first, then Postgres timestamptz with sub-second precision.
	t, err := time.Parse(time.RFC3339, *bannedUntil)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.999999Z07:00", *bannedUntil)
		if err != nil {
			return false
		}
	}
	return t.After(time.Now())
}

// RegisterWebhook registers POST /auth/webhook on mux.
// The route is unauthenticated at the HTTP level — HMAC verification is done
// inside the handler. Must be registered BEFORE wrapping /mcp with auth.Middleware.
// The cache parameter is the same RevocationCache instance used by auth.Middleware
// so that a webhook event immediately invalidates the cached revocation state.
func RegisterWebhook(mux *http.ServeMux, pool *pgxpool.Pool, cache *auth.RevocationCache) {
	h := newWebhookHandler(pool, cache)
	mux.Handle("/auth/webhook", h)
}
