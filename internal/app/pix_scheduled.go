package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/payment"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// PixScheduledCreateEndpoint is the billable endpoint key for creating a PIX charge
// with a due date (cobrança com vencimento, roteiro 7.5). Like pix.create it anchors
// per-tenant pricing; a tenant without a configured price for it cannot create a
// scheduled charge.
const PixScheduledCreateEndpoint = "pix.scheduled.create"

// maxScheduledValidityDays bounds validadeAposVencimento so an unbounded value cannot
// be pushed to the PSP. ~10 years comfortably covers any legitimate window.
const maxScheduledValidityDays = 3650

// CreateScheduledChargeInput is the validated boundary input for creating a scheduled
// PIX charge (roteiro 7.5). TenantID is the authenticated tenant. Unlike the immediate
// charge, a cobv REQUIRES a named devedor (DebtorTaxID + DebtorName). DueDate is the
// dataDeVencimento; ValidityAfterDueDays is the validadeAposVencimento (days).
type CreateScheduledChargeInput struct {
	TenantID             string
	AmountCents          int64
	Currency             string
	IdempotencyKey       string
	DebtorTaxID          string
	DebtorName           string
	DueDate              time.Time
	ValidityAfterDueDays int
}

// CreateScheduledCharge creates a pending scheduled PIX charge at the bank and records
// the billable event, returning the persisted payment together with the charge's QR
// material. It mirrors CreateImmediateCharge's financial-integrity ordering (reserve
// the idempotency key BEFORE the bank call, then persist the bank tx id and the ledger
// entry atomically) so a ledger failure can never leave a charged-but-unbilled
// payment, and a retry with the same key returns the original payment without charging
// again.
func (s *PixService) CreateScheduledCharge(ctx context.Context, in CreateScheduledChargeInput) (*payment.Payment, ports.PixChargeResult, error) {
	t, err := s.tenants.FindTenantByID(ctx, in.TenantID)
	if err != nil {
		return nil, ports.PixChargeResult{}, fmt.Errorf("resolve tenant: %w", err)
	}
	if !t.Active() {
		return nil, ports.PixChargeResult{}, shared.NewValidationError("tenant", "tenant is not active")
	}
	if in.IdempotencyKey == "" {
		return nil, ports.PixChargeResult{}, shared.NewValidationError("idempotency_key", "idempotency key is required")
	}
	if err := validateRequiredDebtor(in.DebtorTaxID, in.DebtorName); err != nil {
		return nil, ports.PixChargeResult{}, err
	}
	if err := validateScheduledCalendar(in.DueDate, in.ValidityAfterDueDays); err != nil {
		return nil, ports.PixChargeResult{}, err
	}

	amount, err := shared.NewMoney(in.AmountCents, in.Currency)
	if err != nil {
		return nil, ports.PixChargeResult{}, err
	}

	price, err := s.pricing.GetEndpointPrice(ctx, in.TenantID, PixScheduledCreateEndpoint)
	if err != nil {
		return nil, ports.PixChargeResult{}, fmt.Errorf("resolve price: %w", err)
	}

	p, err := s.reserveScheduledPayment(ctx, in.TenantID, in.IdempotencyKey, amount)
	if err != nil {
		return nil, ports.PixChargeResult{}, err
	}
	if p.TxID() != "" {
		// Already created and billed by a prior request with this key: re-read the
		// authoritative state so the caller gets a consistent response.
		qr, err := s.scheduled.GetScheduledCharge(ctx, in.TenantID, p.TxID())
		if err != nil {
			return nil, ports.PixChargeResult{}, fmt.Errorf("reconcile existing scheduled charge: %w", err)
		}
		return p, qr, nil
	}

	qr, err := s.scheduled.CreateScheduledCharge(ctx, in.TenantID, ports.ScheduledChargeRequest{
		TenantID:             in.TenantID,
		PaymentID:            p.ID(),
		AmountCents:          p.Amount().Cents(),
		Currency:             p.Amount().Currency(),
		IdempotencyKey:       in.IdempotencyKey,
		DebtorTaxID:          strings.TrimSpace(in.DebtorTaxID),
		DebtorName:           strings.TrimSpace(in.DebtorName),
		DueDate:              in.DueDate,
		ValidityAfterDueDays: in.ValidityAfterDueDays,
	})
	if err != nil {
		return nil, ports.PixChargeResult{}, fmt.Errorf("bank create scheduled charge: %w", err)
	}
	p.SetTxID(qr.TxID)

	entry, err := billing.NewLedgerEntry(s.ids.NewID(), in.TenantID, PixScheduledCreateEndpoint, p.ID(), price.PriceCents(), s.clock.Now())
	if err != nil {
		return nil, ports.PixChargeResult{}, err
	}
	finalized := false
	if err := s.uow.WithinTx(ctx, func(r ports.Repository) error {
		current, lookupErr := r.FindPaymentByIdempotencyKey(ctx, in.TenantID, in.IdempotencyKey)
		if lookupErr == nil && current.TxID() != "" {
			p = current // already finalized and billed by a concurrent/earlier attempt
			return nil
		}
		if lookupErr != nil && !errors.Is(lookupErr, shared.ErrNotFound) {
			return fmt.Errorf("reload payment: %w", lookupErr)
		}
		if err := r.SavePayment(ctx, p); err != nil {
			return fmt.Errorf("save payment: %w", err)
		}
		if err := r.AppendLedgerEntry(ctx, entry); err != nil {
			return fmt.Errorf("append ledger: %w", err)
		}
		finalized = true
		return nil
	}); err != nil {
		return nil, ports.PixChargeResult{}, err
	}

	if finalized {
		s.publishPaymentEvent(ctx, TopicPaymentCreated, p)
	}
	return p, qr, nil
}

// GetScheduledCharge reconciles the authoritative state of a scheduled PIX charge from
// the bank for the authenticated tenant (roteiro 7.7). An unknown txid surfaces as
// not-found (no cross-tenant disclosure: the bank read is tenant-scoped).
func (s *PixService) GetScheduledCharge(ctx context.Context, tenantID, txID string) (ports.PixChargeResult, error) {
	txID = strings.TrimSpace(txID)
	if txID == "" {
		return ports.PixChargeResult{}, shared.NewValidationError("txid", "txid is required")
	}
	return s.scheduled.GetScheduledCharge(ctx, tenantID, txID)
}

// ListScheduledChargesInput is the validated boundary input for listing scheduled PIX
// charges by date window (roteiro 7.6). Page/PageSize are optional.
type ListScheduledChargesInput struct {
	TenantID string
	Start    time.Time
	End      time.Time
	Page     int
	PageSize int
}

// ListScheduledCharges lists the tenant's scheduled PIX charges within the date
// window. The window is mandatory and bounded (end after start, range <=
// maxPixListRange); pagination must be non-negative.
func (s *PixService) ListScheduledCharges(ctx context.Context, in ListScheduledChargesInput) (ports.PixChargeList, error) {
	if err := validatePixListWindow(in.Start, in.End, in.Page, in.PageSize); err != nil {
		return ports.PixChargeList{}, err
	}
	return s.scheduled.ListScheduledCharges(ctx, in.TenantID, ports.PixListFilter{
		Start:    in.Start,
		End:      in.End,
		Page:     in.Page,
		PageSize: in.PageSize,
	})
}

// RegisterPixWebhook configures the URL the PSP notifies when a PIX is received on the
// tenant's PIX key (chave), for the authenticated tenant (roteiro 7.8). It is a
// configuration write (not a charge) so it does not bill. The URL must be a syntactically
// valid absolute https URL; it is validated here and never logged (the adapter keeps it
// out of logs/errors/URLs too — defense in depth).
func (s *PixService) RegisterPixWebhook(ctx context.Context, tenantID, chave, webhookURL string) error {
	chave = strings.TrimSpace(chave)
	if chave == "" {
		return shared.NewValidationError("chave", "pix key (chave) is required")
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if err := validateWebhookURL(webhookURL); err != nil {
		return err
	}
	return s.webhook.RegisterPixWebhook(ctx, tenantID, chave, webhookURL)
}

// reserveScheduledPayment returns the payment to bill for this scheduled charge: an
// existing one for the idempotency key (returned to be completed/resumed), or a freshly
// persisted pending payment reserving the key. On a uniqueness race the winner is
// returned (no double charge). It mirrors reservePayment for the scheduled endpoint.
func (s *PixService) reserveScheduledPayment(ctx context.Context, tenantID, idemKey string, amount shared.Money) (*payment.Payment, error) {
	existing, err := s.payments.FindPaymentByIdempotencyKey(ctx, tenantID, idemKey)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, shared.ErrNotFound) {
		return nil, fmt.Errorf("idempotency lookup: %w", err)
	}

	p, err := payment.New(s.ids.NewID(), tenantID, PixScheduledCreateEndpoint, idemKey, amount, s.clock.Now())
	if err != nil {
		return nil, err
	}
	err = s.uow.WithinTx(ctx, func(r ports.Repository) error {
		return r.SavePayment(ctx, p)
	})
	if err == nil {
		return p, nil
	}
	if errors.Is(err, shared.ErrConflict) {
		won, lookupErr := s.payments.FindPaymentByIdempotencyKey(ctx, tenantID, idemKey)
		if lookupErr != nil {
			return nil, fmt.Errorf("resolve idempotency conflict: %w", lookupErr)
		}
		return won, nil
	}
	return nil, fmt.Errorf("reserve payment: %w", err)
}

// validateRequiredDebtor enforces the mandatory cobv devedor (roteiro 7.5): both a
// syntactically valid CPF/CNPJ tax id and a non-empty name are required. It is the
// stricter sibling of validateDebtor (where the devedor is optional).
func validateRequiredDebtor(taxID, name string) error {
	taxID = strings.TrimSpace(taxID)
	name = strings.TrimSpace(name)
	if name == "" {
		return shared.NewValidationError("devedor.name", "debtor name is required for a scheduled charge")
	}
	if !validTaxID(taxID) {
		return shared.NewValidationError("devedor.tax_id", "debtor tax id must be 11 (CPF) or 14 (CNPJ) digits")
	}
	return nil
}

// validateScheduledCalendar enforces the cobv calendar: a non-zero due date and a
// validity window within [0, maxScheduledValidityDays].
func validateScheduledCalendar(dueDate time.Time, validityDays int) error {
	if dueDate.IsZero() {
		return shared.NewValidationError("due_date", "due date is required")
	}
	if validityDays < 0 || validityDays > maxScheduledValidityDays {
		return shared.NewValidationError("validity_after_due_days", "validity after due date is out of range")
	}
	return nil
}

// validatePixListWindow enforces the shared listing constraints used by both the
// immediate and scheduled PIX list endpoints: a mandatory, ordered, bounded date
// window and non-negative pagination.
func validatePixListWindow(start, end time.Time, page, pageSize int) error {
	if start.IsZero() || end.IsZero() {
		return shared.NewValidationError("start_end", "start and end are required")
	}
	if !end.After(start) {
		return shared.NewValidationError("end", "end must be after start")
	}
	if end.Sub(start) > maxPixListRange {
		return shared.NewValidationError("range", "date range too large")
	}
	if page < 0 || pageSize < 0 {
		return shared.NewValidationError("pagination", "page and page_size must not be negative")
	}
	return nil
}

// validateWebhookURL requires the notification target to be a syntactically valid
// absolute https URL. A non-https or malformed callback is rejected at the boundary so
// a PIX-received notification can never be sent in clear text.
func validateWebhookURL(raw string) error {
	if raw == "" {
		return shared.NewValidationError("webhook_url", "webhook url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return shared.NewValidationError("webhook_url", "webhook url must be an absolute https URL")
	}
	return nil
}
