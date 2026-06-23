package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/payment"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// newScheduledPixHarness builds a harness with the stub wired as the scheduled-charge
// and webhook provider plus a seeded, priced, credentialed tenant.
func newScheduledPixHarness(t *testing.T) (*app.PixService, *harness, string) {
	t.Helper()
	h := newHarness(t)
	h.deps.Pix = h.bank
	h.deps.PixScheduled = h.bank
	h.deps.PixWebhook = h.bank
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	if _, err := admin.SetEndpointPrice(context.Background(), tn.ID(), app.PixScheduledCreateEndpoint, 70); err != nil {
		t.Fatalf("price: %v", err)
	}
	return app.NewPixService(h.deps), h, tn.ID()
}

func validScheduledInput(tenantID, idem string) app.CreateScheduledChargeInput {
	return app.CreateScheduledChargeInput{
		TenantID:             tenantID,
		AmountCents:          2500,
		Currency:             "BRL",
		IdempotencyKey:       idem,
		DebtorTaxID:          "12345678901",
		DebtorName:           "Maria Silva",
		DueDate:              time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		ValidityAfterDueDays: 30,
	}
}

func TestScheduledCreateBillsOnce(t *testing.T) {
	t.Parallel()
	svc, h, tenantID := newScheduledPixHarness(t)

	p, qr, err := svc.CreateScheduledCharge(context.Background(), validScheduledInput(tenantID, "k1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Status() != payment.StatusPending || p.TxID() == "" {
		t.Fatalf("payment not created: %+v", p)
	}
	if qr.TxID != p.TxID() || qr.Status != "ATIVA" || qr.QRCodePayload == "" {
		t.Fatalf("unexpected qr: %+v", qr)
	}
	if h.store.LedgerLen() != 1 {
		t.Fatalf("expected 1 ledger entry, got %d", h.store.LedgerLen())
	}

	// Idempotent retry: same key, no second bill, same txid.
	p2, _, err := svc.CreateScheduledCharge(context.Background(), validScheduledInput(tenantID, "k1"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if p2.TxID() != p.TxID() {
		t.Fatalf("retry changed txid: %q vs %q", p2.TxID(), p.TxID())
	}
	if h.store.LedgerLen() != 1 {
		t.Fatalf("idempotent retry should not bill again, got %d", h.store.LedgerLen())
	}
}

func TestScheduledCreateValidation(t *testing.T) {
	t.Parallel()
	svc, _, tenantID := newScheduledPixHarness(t)

	cases := []struct {
		name string
		in   app.CreateScheduledChargeInput
	}{
		{"missing idem", func() app.CreateScheduledChargeInput { in := validScheduledInput(tenantID, ""); return in }()},
		{"missing debtor name", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.DebtorName = ""
			return in
		}()},
		{"bad debtor taxid", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.DebtorTaxID = "123"
			return in
		}()},
		{"missing debtor taxid", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.DebtorTaxID = ""
			return in
		}()},
		{"zero due date", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.DueDate = time.Time{}
			return in
		}()},
		{"validity too large", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.ValidityAfterDueDays = 100000
			return in
		}()},
		{"negative validity", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.ValidityAfterDueDays = -1
			return in
		}()},
		{"bad currency", func() app.CreateScheduledChargeInput {
			in := validScheduledInput(tenantID, "k1")
			in.Currency = "US"
			return in
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := svc.CreateScheduledCharge(context.Background(), tc.in); !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}

func TestScheduledCreateUnknownTenant(t *testing.T) {
	t.Parallel()
	svc, _, _ := newScheduledPixHarness(t)
	if _, _, err := svc.CreateScheduledCharge(context.Background(), validScheduledInput("missing", "k1")); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown tenant, got %v", err)
	}
}

func TestScheduledCreateUnpricedTenant(t *testing.T) {
	t.Parallel()
	// A tenant without a configured scheduled-charge price cannot create one.
	h := newHarness(t)
	h.deps.Pix = h.bank
	h.deps.PixScheduled = h.bank
	h.deps.PixWebhook = h.bank
	admin := app.NewAdminService(h.deps)
	tn, _ := admin.CreateTenant(context.Background(), "Acme")
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	svc := app.NewPixService(h.deps)
	if _, _, err := svc.CreateScheduledCharge(context.Background(), validScheduledInput(tn.ID(), "k1")); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound for unpriced endpoint, got %v", err)
	}
}

func TestScheduledGet(t *testing.T) {
	t.Parallel()
	svc, _, tenantID := newScheduledPixHarness(t)
	p, _, err := svc.CreateScheduledCharge(context.Background(), validScheduledInput(tenantID, "k1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.GetScheduledCharge(context.Background(), tenantID, p.TxID())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TxID != p.TxID() {
		t.Fatalf("txid mismatch: %q vs %q", got.TxID, p.TxID())
	}
	if _, err := svc.GetScheduledCharge(context.Background(), tenantID, " "); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("blank txid: want ErrValidation, got %v", err)
	}
	if _, err := svc.GetScheduledCharge(context.Background(), tenantID, "missing"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown txid: want ErrNotFound, got %v", err)
	}
}

func TestScheduledList(t *testing.T) {
	t.Parallel()
	svc, _, tenantID := newScheduledPixHarness(t)
	for _, k := range []string{"k1", "k2"} {
		if _, _, err := svc.CreateScheduledCharge(context.Background(), validScheduledInput(tenantID, k)); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	// The stub clock defaults to time.Now; widen the window generously.
	now := time.Now()
	list, err := svc.ListScheduledCharges(context.Background(), app.ListScheduledChargesInput{
		TenantID: tenantID, Start: now.Add(-time.Hour), End: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list.TotalItems != 2 {
		t.Fatalf("want 2 charges, got %d", list.TotalItems)
	}

	// Validation: window required / ordered / non-negative pagination.
	bad := []app.ListScheduledChargesInput{
		{TenantID: tenantID},
		{TenantID: tenantID, Start: now, End: now.Add(-time.Hour)},
		{TenantID: tenantID, Start: now.Add(-time.Hour), End: now, Page: -1},
		{TenantID: tenantID, Start: now.Add(-10000 * time.Hour), End: now},
	}
	for i, in := range bad {
		if _, err := svc.ListScheduledCharges(context.Background(), in); !errors.Is(err, shared.ErrValidation) {
			t.Fatalf("bad[%d]: want ErrValidation, got %v", i, err)
		}
	}
}

func TestRegisterPixWebhookUseCase(t *testing.T) {
	t.Parallel()
	svc, h, tenantID := newScheduledPixHarness(t)

	if err := svc.RegisterPixWebhook(context.Background(), tenantID, "chave-1", "https://hooks.acme.example/pix"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got, ok := h.bank.WebhookURL(tenantID, "chave-1"); !ok || got != "https://hooks.acme.example/pix" {
		t.Fatalf("webhook not recorded: got %q ok=%v", got, ok)
	}

	cases := []struct {
		name, chave, url string
	}{
		{"missing chave", "", "https://hooks.acme.example/pix"},
		{"missing url", "chave", ""},
		{"non-https url", "chave", "http://insecure.example/pix"},
		{"malformed url", "chave", "://bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.RegisterPixWebhook(context.Background(), tenantID, tc.chave, tc.url); !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}
