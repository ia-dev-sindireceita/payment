package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// bankStatusPaid is the authoritative bank status that settles a payment.
const bankStatusPaid = "paid"

// WebhookService processes bank webhook events. A webhook is only a trigger: the
// service is idempotent (anti-replay) and reconciles the authoritative state
// with the bank before settling any payment (threats W2/W3).
type WebhookService struct {
	processed ports.ProcessedEventStore
	payments  ports.PaymentRepository
	bank      ports.BankProvider
	bus       ports.MessageBus
	clock     ports.Clock
}

// NewWebhookService wires a WebhookService from the provided ports.
func NewWebhookService(d Deps) *WebhookService {
	return &WebhookService{
		processed: d.Processed,
		payments:  d.Payments,
		bank:      d.Bank,
		bus:       d.Bus,
		clock:     d.Clock,
	}
}

// PaymentEvent is the validated webhook payload (after transport auth). EventKey
// uniquely identifies the delivery for idempotency (e.g. endToEndId+event).
type PaymentEvent struct {
	TenantID string
	TxID     string
	EventKey string
}

// HandlePaymentEvent reconciles and settles a payment. Duplicate deliveries are
// acked without side effects. The webhook payload is never trusted as financial
// truth — settlement requires a positive reconciliation with the bank.
func (s *WebhookService) HandlePaymentEvent(ctx context.Context, ev PaymentEvent) error {
	if strings.TrimSpace(ev.TenantID) == "" {
		return shared.NewValidationError("tenant_id", "tenant id is required")
	}
	if strings.TrimSpace(ev.TxID) == "" {
		return shared.NewValidationError("tx_id", "tx id is required")
	}
	if strings.TrimSpace(ev.EventKey) == "" {
		return shared.NewValidationError("event_key", "event key is required")
	}

	// Anti-replay: first-time wins, duplicates are acked as no-ops.
	first, err := s.processed.MarkProcessed(ctx, ev.TenantID, ev.EventKey)
	if err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	if !first {
		return nil
	}

	// Reconcile authoritative state with the bank (never trust the raw webhook).
	res, err := s.bank.GetCharge(ctx, ev.TenantID, ev.TxID)
	if err != nil {
		return fmt.Errorf("reconcile charge: %w", err)
	}
	if !strings.EqualFold(res.Status, bankStatusPaid) {
		// Not settled yet; nothing to do but the event is recorded as processed.
		return nil
	}

	p, err := s.payments.FindPaymentByTxID(ctx, ev.TenantID, ev.TxID)
	if err != nil {
		return fmt.Errorf("find payment by tx: %w", err)
	}
	if err := p.MarkPaid(ev.TxID, s.clock.Now()); err != nil {
		if errors.Is(err, shared.ErrConflict) {
			return nil // already settled with a different txid is a conflict we ignore on replay
		}
		return fmt.Errorf("settle payment: %w", err)
	}
	if err := s.payments.SavePayment(ctx, p); err != nil {
		return fmt.Errorf("save settled payment: %w", err)
	}

	payload := []byte(fmt.Sprintf(`{"payment_id":%q,"tenant_id":%q,"tx_id":%q}`, p.ID(), p.TenantID(), p.TxID()))
	_ = s.bus.Publish(ctx, TopicPaymentPaid, ports.Message{
		TenantID:       p.TenantID(),
		Type:           TopicPaymentPaid,
		IdempotencyKey: ev.EventKey,
		Payload:        payload,
	})
	return nil
}
