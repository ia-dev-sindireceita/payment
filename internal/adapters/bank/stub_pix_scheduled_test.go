package bank_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

func schedReq(idem string, amount int64) ports.ScheduledChargeRequest {
	return ports.ScheduledChargeRequest{
		TenantID:             "t1",
		PaymentID:            "pay-" + idem,
		IdempotencyKey:       idem,
		AmountCents:          amount,
		Currency:             "BRL",
		DebtorTaxID:          "12345678901",
		DebtorName:           "Maria",
		DueDate:              time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		ValidityAfterDueDays: 30,
	}
}

func TestStubScheduledCreateAndGet(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	p.SetClock(func() time.Time { return now })
	ctx := context.Background()

	res, err := p.CreateScheduledCharge(ctx, "t1", schedReq("idem1", 2500))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.TxID == "" || res.Status != "ATIVA" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.QRCodePayload == "" || res.QRCodeLocation == "" {
		t.Fatalf("QR material missing: %+v", res)
	}
	if res.ExpectedAmountCents != 2500 {
		t.Fatalf("expected amount: %d", res.ExpectedAmountCents)
	}

	got, err := p.GetScheduledCharge(ctx, "t1", res.TxID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TxID != res.TxID {
		t.Fatalf("get txid mismatch: %q vs %q", got.TxID, res.TxID)
	}
}

// A scheduled charge and an immediate charge must live in SEPARATE stub stores: a
// txid created on /cobv is not readable via the immediate /cob get, and vice versa.
func TestStubScheduledIsolatedFromImmediate(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	ctx := context.Background()

	sched, err := p.CreateScheduledCharge(ctx, "t1", schedReq("idem1", 2500))
	if err != nil {
		t.Fatalf("create scheduled: %v", err)
	}
	if _, err := p.GetImmediateCharge(ctx, "t1", sched.TxID); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("scheduled txid must not resolve via immediate get, got %v", err)
	}
}

func TestStubScheduledIdempotentResubmit(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	ctx := context.Background()

	first, err := p.CreateScheduledCharge(ctx, "t1", schedReq("idem1", 2500))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := p.CreateScheduledCharge(ctx, "t1", schedReq("idem1", 2500))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.TxID != second.TxID {
		t.Fatalf("re-submit changed txid: %q vs %q", first.TxID, second.TxID)
	}
}

func TestStubScheduledValidation(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	ctx := context.Background()

	cases := []struct {
		name string
		req  ports.ScheduledChargeRequest
	}{
		{"empty anchor", ports.ScheduledChargeRequest{TenantID: "t1", AmountCents: 100, Currency: "BRL", DueDate: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)}},
		{"zero amount", func() ports.ScheduledChargeRequest { r := schedReq("idem1", 0); return r }()},
		{"negative amount", func() ports.ScheduledChargeRequest { r := schedReq("idem1", -5); return r }()},
		{"zero due date", func() ports.ScheduledChargeRequest { r := schedReq("idem1", 100); r.DueDate = time.Time{}; return r }()},
		{"negative validity", func() ports.ScheduledChargeRequest {
			r := schedReq("idem1", 100)
			r.ValidityAfterDueDays = -1
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.CreateScheduledCharge(ctx, "t1", tc.req); !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}

func TestStubScheduledGetUnknown(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	if _, err := p.GetScheduledCharge(context.Background(), "t1", "missing"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStubScheduledListByWindowAndPaging(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	p.SetClock(func() time.Time { return now })
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if _, err := p.CreateScheduledCharge(ctx, "t1", schedReq(k, 100)); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	window := ports.PixListFilter{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	all, err := p.ListScheduledCharges(ctx, "t1", window)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if all.TotalItems != 3 || len(all.Charges) != 3 {
		t.Fatalf("want 3 charges, got total=%d len=%d", all.TotalItems, len(all.Charges))
	}

	// Page 0 of size 2 returns the first 2; total pages = 2.
	paged, err := p.ListScheduledCharges(ctx, "t1", ports.PixListFilter{Start: window.Start, End: window.End, Page: 0, PageSize: 2})
	if err != nil {
		t.Fatalf("list paged: %v", err)
	}
	if len(paged.Charges) != 2 || paged.TotalItems != 3 || paged.TotalPages != 2 {
		t.Fatalf("paged: len=%d total=%d pages=%d", len(paged.Charges), paged.TotalItems, paged.TotalPages)
	}

	// A window before the charges excludes them.
	none, err := p.ListScheduledCharges(ctx, "t1", ports.PixListFilter{Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatalf("list none: %v", err)
	}
	if none.TotalItems != 0 {
		t.Fatalf("want 0 charges outside window, got %d", none.TotalItems)
	}
}

func TestStubScheduledListMissingWindow(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	if _, err := p.ListScheduledCharges(context.Background(), "t1", ports.PixListFilter{}); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("want ErrValidation for empty window, got %v", err)
	}
}

func TestStubScheduledRequiresCredential(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	ctx := context.Background()
	if _, err := p.CreateScheduledCharge(ctx, "unknown", schedReq("idem1", 100)); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("create without credential: want ErrNotFound, got %v", err)
	}
	if _, err := p.GetScheduledCharge(ctx, "unknown", "tx"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("get without credential: want ErrNotFound, got %v", err)
	}
	if _, err := p.ListScheduledCharges(ctx, "unknown", ports.PixListFilter{Start: time.Now(), End: time.Now().Add(time.Hour)}); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("list without credential: want ErrNotFound, got %v", err)
	}
}

func TestStubRegisterWebhook(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	ctx := context.Background()

	if err := p.RegisterPixWebhook(ctx, "t1", "chave-1", "https://hooks.example/pix"); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := p.WebhookURL("t1", "chave-1")
	if !ok || got != "https://hooks.example/pix" {
		t.Fatalf("webhook not recorded: got %q ok=%v", got, ok)
	}

	// Idempotent overwrite on the same chave.
	if err := p.RegisterPixWebhook(ctx, "t1", "chave-1", "https://hooks.example/pix2"); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, _ = p.WebhookURL("t1", "chave-1")
	if got != "https://hooks.example/pix2" {
		t.Fatalf("overwrite failed: %q", got)
	}
}

func TestStubRegisterWebhookValidation(t *testing.T) {
	t.Parallel()
	p := newPixStub(t)
	ctx := context.Background()
	if err := p.RegisterPixWebhook(ctx, "t1", "", "https://hooks.example/pix"); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty chave: want ErrValidation, got %v", err)
	}
	if err := p.RegisterPixWebhook(ctx, "t1", "chave", ""); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty url: want ErrValidation, got %v", err)
	}
	if err := p.RegisterPixWebhook(ctx, "unknown", "chave", "https://hooks.example/pix"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown tenant: want ErrNotFound, got %v", err)
	}
	if _, ok := p.WebhookURL("t1", "chave"); ok {
		t.Fatalf("invalid registration must not persist")
	}
}
