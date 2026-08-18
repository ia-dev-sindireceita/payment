package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/webhookref"
)

// revokeRespView mirrors the admin revoke response body.
type revokeRespView struct {
	TenantID string `json:"tenant_id"`
	Revoked  int    `json:"revoked"`
}

// newRevokeServer builds a server whose admin plane exposes the webhook-ref revoke route
// over refStore, with the SAME store wired as the webhook authenticator so a test can
// prove a revoked ref stops authenticating. revokerWired=false leaves the revoker nil to
// exercise the fail-closed (503) path.
func newRevokeServer(refStore *persistence.WebhookRefStore, revokerWired bool) http.Handler {
	auth := httpadapter.NewStaticTokenAuth(nil, []string{adminToken}, nil).WithWebhookRefStore(refStore)
	cfg := httpadapter.Config{
		TenantAuth:  auth,
		AdminAuth:   auth,
		WebhookAuth: auth,
	}
	if revokerWired {
		cfg.WebhookRefRevoker = app.NewWebhookRefRevocationService(refStore)
	}
	return httpadapter.NewServer(cfg).Router()
}

// seedRef mints a ref for tenantID directly in the store and returns the plaintext so a
// test can assert it authenticates before revocation.
func seedRef(t *testing.T, store *persistence.WebhookRefStore, tenantID string) string {
	t.Helper()
	ref, err := webhookref.Generate()
	if err != nil {
		t.Fatalf("generate ref: %v", err)
	}
	sum := webhookref.Sum(ref)
	if err := store.PutWebhookRef(context.Background(), sum[:], tenantID); err != nil {
		t.Fatalf("seed ref: %v", err)
	}
	return ref
}

// TestAdminRevokeWebhookRefsSoftDeletes is the SIN-69584 / B1 acceptance: an admin
// revokes a tenant's active refs and the (previously authenticating) ref then resolves
// as the SAME non-oracle miss as an unregistered one — a revoked ref stops
// authenticating the inbound callback, no enumeration signal.
func TestAdminRevokeWebhookRefsSoftDeletes(t *testing.T) {
	t.Parallel()
	store := persistence.NewWebhookRefStore()
	ref := seedRef(t, store, "emp-42")
	handler := newRevokeServer(store, true)

	// Before revocation the ref authenticates to its tenant.
	authCheck := httpadapter.NewStaticTokenAuth(nil, nil, nil).WithWebhookRefStore(store)
	if id, ok := authCheck.AuthenticateWebhook(ref); !ok || id.TenantID != "emp-42" {
		t.Fatalf("pre-revoke auth = (%+v, %v), want emp-42/true", id, ok)
	}

	rec := do(t, handler, http.MethodPost, "/admin/tenants/emp-42/webhook-refs/revoke", adminToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var v revokeRespView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.TenantID != "emp-42" || v.Revoked != 1 {
		t.Fatalf("revoke result = %+v, want emp-42/1", v)
	}
	// After revocation the same ref no longer authenticates (non-oracle miss).
	if id, ok := authCheck.AuthenticateWebhook(ref); ok {
		t.Fatalf("revoked ref still authenticates to %+v", id)
	}
}

// TestAdminRevokeWebhookRefsIdempotent proves revoking a tenant with no active ref is a
// no-op success (200, revoked=0) rather than an error — so an operator can revoke safely
// without first probing whether a ref exists (which would be an oracle).
func TestAdminRevokeWebhookRefsIdempotent(t *testing.T) {
	t.Parallel()
	store := persistence.NewWebhookRefStore()
	handler := newRevokeServer(store, true)

	rec := do(t, handler, http.MethodPost, "/admin/tenants/emp-none/webhook-refs/revoke", adminToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var v revokeRespView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Revoked != 0 {
		t.Fatalf("revoked = %d, want 0 for a tenant with no active ref", v.Revoked)
	}
}

// TestAdminRevokeWebhookRefsIsolatedPerTenant proves revocation is tenant-scoped: it
// never touches another tenant's active ref.
func TestAdminRevokeWebhookRefsIsolatedPerTenant(t *testing.T) {
	t.Parallel()
	store := persistence.NewWebhookRefStore()
	_ = seedRef(t, store, "emp-A")
	refB := seedRef(t, store, "emp-B")
	handler := newRevokeServer(store, true)

	rec := do(t, handler, http.MethodPost, "/admin/tenants/emp-A/webhook-refs/revoke", adminToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	authCheck := httpadapter.NewStaticTokenAuth(nil, nil, nil).WithWebhookRefStore(store)
	if id, ok := authCheck.AuthenticateWebhook(refB); !ok || id.TenantID != "emp-B" {
		t.Fatalf("emp-B ref must survive emp-A revoke, got (%+v, %v)", id, ok)
	}
}

// TestAdminRevokeWebhookRefsRequiresAdmin proves the route is deny-by-default: an
// unauthenticated caller is rejected before any revocation runs.
func TestAdminRevokeWebhookRefsRequiresAdmin(t *testing.T) {
	t.Parallel()
	store := persistence.NewWebhookRefStore()
	ref := seedRef(t, store, "emp-42")
	handler := newRevokeServer(store, true)

	rec := do(t, handler, http.MethodPost, "/admin/tenants/emp-42/webhook-refs/revoke", "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revoke = %d, want 401", rec.Code)
	}
	// The ref must be untouched (no revocation happened behind the auth gate).
	authCheck := httpadapter.NewStaticTokenAuth(nil, nil, nil).WithWebhookRefStore(store)
	if _, ok := authCheck.AuthenticateWebhook(ref); !ok {
		t.Fatal("unauthenticated request must not revoke the ref")
	}
}

// TestAdminRevokeWebhookRefsUnavailable proves the route fails CLOSED (503) when no
// revoker is wired, rather than reporting a success it cannot guarantee.
func TestAdminRevokeWebhookRefsUnavailable(t *testing.T) {
	t.Parallel()
	store := persistence.NewWebhookRefStore()
	handler := newRevokeServer(store, false)

	rec := do(t, handler, http.MethodPost, "/admin/tenants/emp-42/webhook-refs/revoke", adminToken, nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-revoker revoke = %d, want 503", rec.Code)
	}
}
