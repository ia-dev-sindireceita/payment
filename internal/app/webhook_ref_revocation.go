package app

import (
	"context"
	"errors"
	"strings"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// ErrWebhookRefRevocationUnavailable is returned when the durable ref store is not
// wired. It mirrors the other "store not configured" sentinels so a stripped-down
// deployment fails closed with a clear error instead of a nil-pointer panic.
var ErrWebhookRefRevocationUnavailable = errors.New("webhook ref store not configured")

// webhookRefRevoker is the narrow slice of ports.WebhookRefStore the revocation
// use-case needs (accept-narrow): soft-delete a tenant's active refs. Satisfied by the
// in-memory and sqlite adapters.
type webhookRefRevoker interface {
	RevokeWebhookRefs(ctx context.Context, tenantID string) (revoked int, err error)
}

// WebhookRefRevocationService is the use-case behind admin-plane, tenant-scoped webhook
// ref revocation (SIN-69584 / B1 — the revocation path of SIN-69583). Because the store
// is hash-only, an operator cannot present the plaintext of a leaked/orphan ref (e.g.
// the Verz orphan), so revocation is tenant-scoped: it soft-deletes EVERY active ref for
// a tenant. A subsequent in-flow registration (SIN-69560 / F2) mints a fresh clean ref.
//
// SECURITY: soft-delete (revoked_at) means a revoked ref resolves as a non-oracle miss
// on the inbound path — identical to an unregistered ref (same uniform 401/404), no
// enumeration signal. Rows are kept for audit, not deleted. The operation is idempotent:
// revoking a tenant with no active ref returns (0, nil).
type WebhookRefRevocationService struct {
	store webhookRefRevoker
}

// NewWebhookRefRevocationService wires the service over a durable ref store. A nil store
// yields a service whose RevokeTenantRefs fails closed (ErrWebhookRefRevocationUnavailable)
// rather than silently reporting success it cannot guarantee.
func NewWebhookRefRevocationService(store webhookRefRevoker) *WebhookRefRevocationService {
	return &WebhookRefRevocationService{store: store}
}

// RevokeTenantRefs soft-deletes every active webhook ref bound to tenantID and returns
// how many were revoked. tenantID is required (a blank one is a validation error, not a
// silent no-op). Idempotent: a tenant with no active ref revokes zero and returns nil.
func (s *WebhookRefRevocationService) RevokeTenantRefs(ctx context.Context, tenantID string) (int, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return 0, shared.NewValidationError("tenant_id", "tenant id is required")
	}
	if s == nil || s.store == nil {
		return 0, ErrWebhookRefRevocationUnavailable
	}
	return s.store.RevokeWebhookRefs(ctx, tenantID)
}
