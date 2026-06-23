package c6

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// --- PIX cobv (cobrança com vencimento, roteiro 7.5–7.7) adapter ---

func cobvReq(tenant string) ports.PixDueChargeRequest {
	return ports.PixDueChargeRequest{
		TenantID: tenant, AmountCents: 1000, Currency: "BRL",
		DueDate: time.Unix(1_900_000_000, 0).UTC(), ValidityDays: 5,
		FineBps: 200, MonthlyInterestBps: 100, DiscountBps: 500,
		DebtorTaxID: "12345678901", DebtorName: "Maria",
		CreditorKey: "acme@pix.example", IdempotencyKey: "cobv-key-1",
	}
}

// roteiro 7.5: create cobv via idempotent PUT; bearer + idempotency forwarded; the
// fine/interest/discount RATES and due date are transported (the domain computes
// amounts, never the adapter).
func TestCreateDueChargeSuccess(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	p := ps.provider(t, oneTenant("t1", "client-1", "secret-1"))

	res, err := p.CreateDueCharge(context.Background(), "t1", cobvReq("t1"))
	if err != nil {
		t.Fatalf("CreateDueCharge: %v", err)
	}
	if res.Status != "ATIVA" || res.QRCodePayload != "pix-cobv-emv" || res.QRCodeLocation == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.ValidityDays != 5 || res.FineBps != 200 || res.MonthlyInterestBps != 100 || res.DiscountBps != 500 {
		t.Fatalf("registered params not reconciled: %+v", res)
	}
	if ps.lastAuthHeader != "Bearer tok-client-1" {
		t.Fatalf("bearer not attached: %q", ps.lastAuthHeader)
	}
	if ps.idemKey() != "cobv-key-1" {
		t.Fatalf("idempotency key not forwarded: %q", ps.idemKey())
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(ps.body(), &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, f := range []string{"due_date", "validity_days", "fine_bps", "monthly_interest_bps", "creditor_key", "debtor_tax_id"} {
		if _, ok := sent[f]; !ok {
			t.Fatalf("cobv request must carry %q, body=%s", f, ps.body())
		}
	}
}

// Complete mediation at the money seam: empty anchor or non-positive amount is
// refused at the boundary without hitting the PSP.
func TestCreateDueChargeRejectsBadInput(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	p := ps.provider(t, oneTenant("t1", "c", "s"))

	// Empty anchor (no idempotency key, no txid).
	bad := cobvReq("t1")
	bad.IdempotencyKey = ""
	bad.TxID = ""
	if _, err := p.CreateDueCharge(context.Background(), "t1", bad); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty anchor must map to ErrValidation, got %v", err)
	}
	// Non-positive amount.
	bad = cobvReq("t1")
	bad.AmountCents = 0
	if _, err := p.CreateDueCharge(context.Background(), "t1", bad); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("zero amount must map to ErrValidation, got %v", err)
	}
}

func TestCreateDueChargeErrorMapping(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	ps.cobvPut = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"BAD"}`))
	}
	p := ps.provider(t, oneTenant("t1", "c", "s"))
	if _, err := p.CreateDueCharge(context.Background(), "t1", cobvReq("t1")); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("400 should map to ErrValidation, got %v", err)
	}
}

// roteiro 7.6: reconcile read; the received amount reconciles against expected.
func TestGetDueChargeSuccess(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	p := ps.provider(t, oneTenant("t1", "client-1", "secret-1"))

	res, err := p.GetDueCharge(context.Background(), "t1", "tx_cobv_1")
	if err != nil {
		t.Fatalf("GetDueCharge: %v", err)
	}
	if res.Status != "CONCLUIDA" || res.ExpectedAmountCents != 1000 || res.ReceivedAmountCents != 1000 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !res.AmountReconciled() {
		t.Fatalf("a fully-paid charge must reconcile: %+v", res)
	}
	if ps.lastAuthHeader != "Bearer tok-client-1" {
		t.Fatalf("bearer not attached: %q", ps.lastAuthHeader)
	}
}

func TestGetDueChargeNotFoundMapping(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	ps.cobvGet = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND"}`))
	}
	p := ps.provider(t, oneTenant("t1", "c", "s"))
	if _, err := p.GetDueCharge(context.Background(), "t1", "nope"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("404 should map to ErrNotFound, got %v", err)
	}
}

// roteiro 7.7: amend via PUT carries the new params; bearer + idempotency forwarded.
func TestUpdateDueChargeSuccess(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	p := ps.provider(t, oneTenant("t1", "client-1", "secret-1"))

	req := cobvReq("t1")
	req.AmountCents = 2000
	req.IdempotencyKey = "upd-key"
	res, err := p.UpdateDueCharge(context.Background(), "t1", "tx_cobv_1", req)
	if err != nil {
		t.Fatalf("UpdateDueCharge: %v", err)
	}
	if res.TxID != "tx_cobv_1" {
		t.Fatalf("amend must address the known txid, got %q", res.TxID)
	}
	var sent struct {
		TxID        string `json:"txid"`
		AmountCents int64  `json:"amount_cents"`
	}
	if err := json.Unmarshal(ps.body(), &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if sent.TxID != "tx_cobv_1" || sent.AmountCents != 2000 {
		t.Fatalf("update body not transported: %s", ps.body())
	}
	if ps.idemKey() != "upd-key" {
		t.Fatalf("idempotency key not forwarded: %q", ps.idemKey())
	}
}

func TestUpdateDueChargeNotFoundMapping(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	ps.cobvPut = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND"}`))
	}
	p := ps.provider(t, oneTenant("t1", "c", "s"))
	req := cobvReq("t1")
	if _, err := p.UpdateDueCharge(context.Background(), "t1", "nope", req); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("404 should map to ErrNotFound, got %v", err)
	}
}

func TestCobvMissingCredential(t *testing.T) {
	t.Parallel()
	ps := newProductServer(t)
	p := ps.provider(t, &fakeCreds{creds: map[string]ports.BankCredential{}})
	if _, err := p.GetDueCharge(context.Background(), "unknown", "tx"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("missing credential should propagate ErrNotFound, got %v", err)
	}
	if ps.tokenCount() != 0 {
		t.Fatalf("token must not be hit without a credential, hits=%d", ps.tokenCount())
	}
}
