package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/payment"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// boletoDueDate is a fixed, non-zero vencimento for the harness (the fixedClock is
// Unix(1000)); the domain only requires a non-zero due date.
var boletoDueDate = time.Unix(1_800_000_000, 0).UTC()

// newBoletoHarness wires a BoletoService with the stub as the boleto provider plus a
// seeded, priced, credentialed tenant.
func newBoletoHarness(t *testing.T) (*app.BoletoService, *harness, string) {
	t.Helper()
	h := newHarness(t)
	h.deps.Boleto = h.bank
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	if _, err := admin.SetEndpointPrice(context.Background(), tn.ID(), app.BoletoCreateEndpoint, 40); err != nil {
		t.Fatalf("price: %v", err)
	}
	return app.NewBoletoService(h.deps), h, tn.ID()
}

func baseBoletoInput(tenantID, idemKey string) app.RegisterBoletoInput {
	return app.RegisterBoletoInput{
		TenantID:           tenantID,
		AmountCents:        100000,
		Currency:           "BRL",
		DueDate:            boletoDueDate,
		FineBps:            200,
		MonthlyInterestBps: 100,
		IdempotencyKey:     idemKey,
	}
}

// roteiro grupos 1–3: register the boleto variants (fine+interest, fine only,
// interest only, fixed/percent fine, until-due discount, tiered discount).
func TestRegisterBoletoVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*app.RegisterBoletoInput)
	}{
		{"1a_fine_and_interest", func(in *app.RegisterBoletoInput) {}},
		{"1b_no_fine_no_interest", func(in *app.RegisterBoletoInput) { in.FineBps = 0; in.MonthlyInterestBps = 0 }},
		{"2a_interest_only", func(in *app.RegisterBoletoInput) { in.FineBps = 0 }},
		{"2b_fine_only", func(in *app.RegisterBoletoInput) { in.MonthlyInterestBps = 0 }},
		{"2c_fine_fixed", func(in *app.RegisterBoletoInput) { in.FineBps = 0; in.FineFixedCents = 1500 }},
		{"3a_discount_until_due", func(in *app.RegisterBoletoInput) {
			in.Discounts = []app.DiscountTierInput{{DaysBeforeDue: 0, Bps: 500}}
		}},
		{"3a_discount_fixed", func(in *app.RegisterBoletoInput) {
			in.Discounts = []app.DiscountTierInput{{DaysBeforeDue: 0, FixedCents: 2500}}
		}},
		{"3b_discount_tiered", func(in *app.RegisterBoletoInput) {
			in.Discounts = []app.DiscountTierInput{
				{DaysBeforeDue: 10, Bps: 1000},
				{DaysBeforeDue: 5, Bps: 500},
				{DaysBeforeDue: 0, Bps: 200},
			}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, h, tenantID := newBoletoHarness(t)
			in := baseBoletoInput(tenantID, "k-"+tc.name)
			tc.mut(&in)
			p, res, err := svc.RegisterBoleto(context.Background(), in)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if p.Status() != payment.StatusPending || p.TxID() == "" {
				t.Fatalf("payment not created: %+v", p)
			}
			if p.Amount().Cents() != 100000 {
				t.Fatalf("principal: want 100000 got %d", p.Amount().Cents())
			}
			if res.BoletoID != p.ID() || res.Status != "REGISTERED" || res.QRCode == "" || res.Barcode == "" {
				t.Fatalf("unexpected result: %+v", res)
			}
			if h.store.LedgerLen() != 1 {
				t.Fatalf("expected 1 ledger entry, got %d", h.store.LedgerLen())
			}
			// The boleto is reconcilable by id (roteiro 6.a).
			got, err := svc.GetBoleto(context.Background(), tenantID, res.BoletoID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.BoletoID != res.BoletoID {
				t.Fatalf("get mismatch: %+v", got)
			}
		})
	}
}

func TestRegisterBoletoIdempotent(t *testing.T) {
	t.Parallel()
	svc, h, tenantID := newBoletoHarness(t)
	in := baseBoletoInput(tenantID, "k1")

	var events int
	var mu sync.Mutex
	_ = h.bus.Subscribe(context.Background(), app.TopicPaymentCreated, func(_ context.Context, _ ports.Message) error {
		mu.Lock()
		events++
		mu.Unlock()
		return nil
	})

	p1, res1, err := svc.RegisterBoleto(context.Background(), in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	p2, res2, err := svc.RegisterBoleto(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if p1.ID() != p2.ID() || res1.BoletoID != res2.BoletoID {
		t.Fatalf("idempotent retry returned different boleto: %s vs %s", res1.BoletoID, res2.BoletoID)
	}
	if h.store.LedgerLen() != 1 {
		t.Fatalf("idempotent retry should not bill again, got %d", h.store.LedgerLen())
	}
	mu.Lock()
	if events != 1 {
		t.Fatalf("expected 1 event, got %d", events)
	}
	mu.Unlock()
}

func TestRegisterBoletoValidationErrors(t *testing.T) {
	t.Parallel()
	svc, _, tenantID := newBoletoHarness(t)

	mut := func(f func(*app.RegisterBoletoInput)) app.RegisterBoletoInput {
		in := baseBoletoInput(tenantID, "k1")
		f(&in)
		return in
	}
	cases := []struct {
		name string
		in   app.RegisterBoletoInput
	}{
		{"zero_amount", mut(func(in *app.RegisterBoletoInput) { in.AmountCents = 0 })},
		{"negative_amount", mut(func(in *app.RegisterBoletoInput) { in.AmountCents = -1 })},
		{"bad_currency", mut(func(in *app.RegisterBoletoInput) { in.Currency = "XX" })},
		{"zero_due_date", mut(func(in *app.RegisterBoletoInput) { in.DueDate = time.Time{} })},
		{"fine_over_cap", mut(func(in *app.RegisterBoletoInput) { in.FineBps = 201 })},
		{"interest_over_cap", mut(func(in *app.RegisterBoletoInput) { in.MonthlyInterestBps = 101 })},
		{"missing_idem", mut(func(in *app.RegisterBoletoInput) { in.IdempotencyKey = "" })},
		{"bad_payer_tax_id", mut(func(in *app.RegisterBoletoInput) { in.PayerTaxID = "123" })},
		{"both_fine_forms", mut(func(in *app.RegisterBoletoInput) { in.FineBps = 200; in.FineFixedCents = 100 })},
		{"fixed_fine_over_cap", mut(func(in *app.RegisterBoletoInput) { in.FineBps = 0; in.FineFixedCents = 9999 })},
		{"discount_both_set", mut(func(in *app.RegisterBoletoInput) {
			in.Discounts = []app.DiscountTierInput{{DaysBeforeDue: 0, Bps: 100, FixedCents: 100}}
		})},
		{"discount_exceeds_principal", mut(func(in *app.RegisterBoletoInput) {
			in.Discounts = []app.DiscountTierInput{{DaysBeforeDue: 0, FixedCents: 100000}}
		})},
		{"discount_not_ordered", mut(func(in *app.RegisterBoletoInput) {
			in.Discounts = []app.DiscountTierInput{
				{DaysBeforeDue: 5, Bps: 200},
				{DaysBeforeDue: 10, Bps: 500},
			}
		})},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := svc.RegisterBoleto(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("want validation error for %s, got nil", tc.name)
			}
			if !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("%s: want validation error, got %v", tc.name, err)
			}
		})
	}
}

// An invalid request must not reserve a payment (fail before the money seam).
func TestRegisterBoletoInvalidDoesNotReserve(t *testing.T) {
	t.Parallel()
	svc, h, tenantID := newBoletoHarness(t)
	in := baseBoletoInput(tenantID, "k1")
	in.FineBps = 201 // over cap
	if _, _, err := svc.RegisterBoleto(context.Background(), in); err == nil {
		t.Fatalf("want validation error")
	}
	if _, err := h.store.FindPaymentByIdempotencyKey(context.Background(), tenantID, "k1"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("an invalid request must not reserve a payment, got %v", err)
	}
	if h.store.LedgerLen() != 0 {
		t.Fatalf("invalid request must not bill, got %d", h.store.LedgerLen())
	}
}

func TestRegisterBoletoInactiveTenant(t *testing.T) {
	t.Parallel()
	svc, h, tenantID := newBoletoHarness(t)
	tn, err := h.store.FindTenantByID(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("find tenant: %v", err)
	}
	tn.Deactivate()
	if err := h.store.SaveTenant(context.Background(), tn); err != nil {
		t.Fatalf("save tenant: %v", err)
	}
	if _, _, err := svc.RegisterBoleto(context.Background(), baseBoletoInput(tenantID, "k1")); err == nil {
		t.Fatalf("want error for inactive tenant")
	}
}

func TestRegisterBoletoUnknownTenant(t *testing.T) {
	t.Parallel()
	svc, _, _ := newBoletoHarness(t)
	if _, _, err := svc.RegisterBoleto(context.Background(), baseBoletoInput("nope", "k1")); err == nil {
		t.Fatalf("want error for unknown tenant")
	}
}

func TestRegisterBoletoNoPrice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Boleto = h.bank
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "NoPrice")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	svc := app.NewBoletoService(h.deps)
	if _, _, err := svc.RegisterBoleto(context.Background(), baseBoletoInput(tn.ID(), "k1")); err == nil {
		t.Fatalf("want error when endpoint price is not configured")
	}
}

// A provider failure (the stub rejects a tenant without a bank credential) must
// surface as an error and never bill.
func TestRegisterBoletoBankError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Boleto = h.bank
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "NoCred")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := admin.SetEndpointPrice(context.Background(), tn.ID(), app.BoletoCreateEndpoint, 40); err != nil {
		t.Fatalf("price: %v", err)
	}
	svc := app.NewBoletoService(h.deps)
	if _, _, err := svc.RegisterBoleto(context.Background(), baseBoletoInput(tn.ID(), "k1")); err == nil {
		t.Fatalf("want error when the provider rejects the request")
	}
	if h.store.LedgerLen() != 0 {
		t.Fatalf("a failed provider call must not bill, got %d ledger entries", h.store.LedgerLen())
	}
}

func TestGetBoletoValidationAndNotFound(t *testing.T) {
	t.Parallel()
	svc, _, tenantID := newBoletoHarness(t)
	if _, err := svc.GetBoleto(context.Background(), tenantID, "  "); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("blank id: want validation, got %v", err)
	}
	if _, err := svc.GetBoleto(context.Background(), tenantID, "missing"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown id: want not found, got %v", err)
	}
}

// Concurrent registers with the same idempotency key must register/bill exactly once.
func TestRegisterBoletoConcurrentSameKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Boleto = h.bank
	h.deps.UoW = h.store // real transactional boundary so the unique index serialises racers
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	if _, err := admin.SetEndpointPrice(context.Background(), tn.ID(), app.BoletoCreateEndpoint, 40); err != nil {
		t.Fatalf("price: %v", err)
	}
	svc := app.NewBoletoService(h.deps)
	in := baseBoletoInput(tn.ID(), "kc")

	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, _, err := svc.RegisterBoleto(context.Background(), in)
			errs[i] = err
			if p != nil {
				ids[i] = p.ID()
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("idempotency violated: id[%d]=%q != id[0]=%q", i, ids[i], ids[0])
		}
	}
	if got := h.store.LedgerLen(); got != 1 {
		t.Fatalf("concurrent same-key registers must bill once: ledger len = %d, want 1", got)
	}
}
