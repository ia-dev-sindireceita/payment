package http

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// WebhookRefRevoker is the narrow app-layer port the admin revoke route depends on
// (accept-narrow): soft-delete every active webhook ref for a tenant and report how many
// were revoked. Satisfied by app.WebhookRefRevocationService. Kept behind an interface so
// the handler never touches the store / SQL directly (hexagonal).
type WebhookRefRevoker interface {
	RevokeTenantRefs(ctx context.Context, tenantID string) (revoked int, err error)
}

// revokeWebhookRefsView is the admin revoke response: the tenant whose refs were revoked
// and the count. It carries NO secret (the store is hash-only; a ref plaintext never
// existed here to leak).
type revokeWebhookRefsView struct {
	TenantID string `json:"tenant_id"`
	Revoked  int    `json:"revoked"`
}

// handleRevokeWebhookRefs is the admin-plane, tenant-scoped webhook ref revocation route
// (POST /admin/tenants/{tenantID}/webhook-refs/revoke, SIN-69584 / B1). It sits behind
// admin Bearer auth + RoleAdmin (deny-by-default, least privilege) and soft-deletes every
// active ref for the tenant so a leaked/orphan ref (the Verz orphan) stops authenticating
// an inbound C6 callback — a revoked ref then resolves as the SAME non-oracle 401/404 miss
// as an unregistered one, no enumeration signal.
//
// Idempotent: revoking a tenant with no active ref returns 200 with revoked=0. The ref
// store is hash-only, so this deliberately revokes ALL of the tenant's active refs rather
// than a specific plaintext the operator cannot present. It is safe to run on the ORPHAN
// ref (which C6 never calls); revoking a ref C6 IS actively calling opens a fail-closed
// window until F2/sweep re-registers, so ordering is revoke-then-reregister (documented in
// the runbook), never a user-facing rotation (the plaintext never leaves the process).
func (s *Server) handleRevokeWebhookRefs(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	if s.webhookRefRevoker == nil {
		// Fail closed rather than pretend success the store cannot guarantee.
		writeError(w, http.StatusServiceUnavailable, "webhook ref revocation unavailable")
		return
	}
	n, err := s.webhookRefRevoker.RevokeTenantRefs(r.Context(), tenantID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revokeWebhookRefsView{TenantID: tenantID, Revoked: n})
}
