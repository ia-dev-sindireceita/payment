package bank

import (
	"context"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// This file extends StubProvider to back the scheduled-charge (cobrança com
// vencimento, BACEN /cobv, roteiro 7.5–7.7) and webhook-registration (roteiro 7.8)
// halves of ports.PixProvider in-memory, so the use-cases and HTTP routes run
// end-to-end in stub mode without C6. The behaviour mirrors the real C6 adapter's
// observable contract: idempotent create keyed by the request's idempotency anchor, a
// deterministic BACEN-shaped txid, an ATIVA charge with QR material, reconcile/list
// reads, and an idempotent webhook registration keyed by the PIX chave. Every call
// resolves the tenant credential first to demonstrate per-tenant isolation; the
// secret and the webhook URL are never logged.

// scheduledAnchor returns the request's idempotency anchor: the IdempotencyKey when
// present, else the PaymentID.
func scheduledAnchor(req ports.ScheduledChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.PaymentID
}

// CreateScheduledCharge creates an ATIVA scheduled PIX charge in-memory and returns
// its QR code. It is idempotent on the request's anchor: a re-submit with the same
// (tenant, anchor) returns the original charge without creating a new one. A
// non-positive amount, an empty anchor or a missing/invalid due date is rejected
// (complete mediation at the money seam, mirroring the C6 adapter).
func (s *StubProvider) CreateScheduledCharge(ctx context.Context, tenantID string, req ports.ScheduledChargeRequest) (ports.PixChargeResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.PixChargeResult{}, err
	}
	anchor := scheduledAnchor(req)
	if anchor == "" || req.AmountCents <= 0 {
		return ports.PixChargeResult{}, shared.ErrValidation
	}
	if req.DueDate.IsZero() || req.ValidityAfterDueDays < 0 {
		return ports.PixChargeResult{}, shared.ErrValidation
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if txID, ok := s.cobvByIdem[key(tenantID, anchor)]; ok {
		return s.cobv[key(tenantID, txID)].result, nil // idempotent re-submit
	}

	now := s.now()
	txID := stubPixTxID(anchor)
	res := ports.PixChargeResult{
		TxID:                txID,
		Status:              "ATIVA",
		QRCodePayload:       "00020126-stub-cobv-" + txID,
		QRCodeLocation:      "https://pix.stub.example/qr/cobv/" + txID,
		ExpectedAmountCents: req.AmountCents,
	}
	s.cobv[key(tenantID, txID)] = stubPixCharge{result: res, createdAt: now}
	s.cobvByIdem[key(tenantID, anchor)] = txID
	return res, nil
}

// GetScheduledCharge returns the authoritative state of a scheduled PIX charge for
// reconciliation. An unknown txid (within the tenant) is shared.ErrNotFound.
func (s *StubProvider) GetScheduledCharge(ctx context.Context, tenantID, txID string) (ports.PixChargeResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.PixChargeResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cobv[key(tenantID, txID)]
	if !ok {
		return ports.PixChargeResult{}, shared.ErrNotFound
	}
	return c.result, nil
}

// ListScheduledCharges returns the tenant's scheduled PIX charges created within
// [Start,End] (inclusive), paginated. Results are ordered by creation instant so
// pagination is stable. PageSize <= 0 returns every match on page 0.
func (s *StubProvider) ListScheduledCharges(ctx context.Context, tenantID string, filter ports.PixListFilter) (ports.PixChargeList, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.PixChargeList{}, err
	}
	if filter.Start.IsZero() || filter.End.IsZero() {
		return ports.PixChargeList{}, shared.ErrValidation
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := tenantID + "\x00"
	matches := make([]stubPixCharge, 0)
	for k, c := range s.cobv {
		if len(k) < len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		if c.createdAt.Before(filter.Start) || c.createdAt.After(filter.End) {
			continue
		}
		matches = append(matches, c)
	}
	sortStubPixCharges(matches)

	total := len(matches)
	pageSize := filter.PageSize
	page := filter.Page
	if pageSize <= 0 {
		pageSize = total
		page = 0
	}
	out := pageStubPixCharges(matches, page, pageSize)

	charges := make([]ports.PixChargeResult, 0, len(out))
	for _, c := range out {
		charges = append(charges, c.result)
	}
	totalPages := 0
	if pageSize > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	return ports.PixChargeList{
		Charges:    charges,
		Page:       page,
		PageSize:   pageSize,
		TotalItems: total,
		TotalPages: totalPages,
	}, nil
}

// RegisterPixWebhook records the notification URL for the tenant's PIX key. It is
// idempotent on (tenant, chave): re-registering overwrites. The URL is stored but
// never logged.
func (s *StubProvider) RegisterPixWebhook(ctx context.Context, tenantID, chave, webhookURL string) error {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return err
	}
	if chave == "" || webhookURL == "" {
		return shared.ErrValidation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhooks[key(tenantID, chave)] = webhookURL
	return nil
}

// WebhookURL returns the URL registered for (tenant, chave) and whether one exists.
// It is a test/dev hook so a stub-mode test can assert the registration took effect
// without reaching into unexported state.
func (s *StubProvider) WebhookURL(tenantID, chave string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.webhooks[key(tenantID, chave)]
	return u, ok
}

// compile-time assertions that the stub satisfies the scheduled-charge and webhook
// ports (in addition to the immediate ports.PixProvider asserted in stub_pix.go).
var (
	_ ports.PixScheduledProvider = (*StubProvider)(nil)
	_ ports.PixWebhookRegistrar  = (*StubProvider)(nil)
)
