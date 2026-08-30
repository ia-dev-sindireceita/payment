package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/recurrence"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// recurrenceNoBankHarness wires only the durable store and leaves every bank-side
// recurrence port (RecReader / SolicRecs / LocRecs / CobRReader) nil, so each
// operation that needs the bank surfaces ErrUnavailable rather than panicking. This
// exercises the fail-closed "port not configured" branch of every method in one place.
func recurrenceNoBankHarness(t *testing.T) *app.RecurrenceService {
	t.Helper()
	h := newHarness(t)
	h.deps.Recs = h.store
	h.deps.CobRs = h.store
	h.deps.Audit = h.store
	h.deps.UoW = h.store
	return app.NewRecurrenceService(h.deps)
}

// approvedMandateWithCharge registers a mandate, drives it to APROVADA through the
// reconciled webhook (the only path that makes a mandate chargeable), and originates
// one recurring charge against it. It returns the mandate's idRec. It mirrors the
// production journey so the read/amend paths are tested against real durable state.
func approvedMandateWithCharge(t *testing.T, svc *app.RecurrenceService, h *harness, tenantID, jornadaTx, cycleTx string, cents int64) string {
	t.Helper()
	ctx := context.Background()
	rec, _, err := svc.CreateMandate(ctx, mandateInput(tenantID, jornadaTx, 0, cents))
	if err != nil {
		t.Fatalf("create mandate: %v", err)
	}
	if _, err := h.bank.ApproveRec(ctx, tenantID, rec.IDRec()); err != nil {
		t.Fatalf("approve at bank: %v", err)
	}
	wh := app.NewWebhookService(h.deps)
	if err := wh.HandleRecEvent(ctx, app.RecEvent{
		TenantID: tenantID, IDRec: rec.IDRec(), EventKey: rec.IDRec() + "|rec|APROVADA",
	}); err != nil {
		t.Fatalf("record approval: %v", err)
	}
	if _, err := svc.OriginateCobR(ctx, app.OriginateCobRInput{
		TenantID: tenantID, IDRec: rec.IDRec(), TxID: cycleTx, Vencimento: "2026-09-01", ValorCents: cents,
	}); err != nil {
		t.Fatalf("originate: %v", err)
	}
	return rec.IDRec()
}

func TestGetMandate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _, tenantID := mandateJourneyHarness(t)
	rec, _, err := svc.CreateMandate(ctx, mandateInput(tenantID, "tx-1", 0, 0))
	if err != nil {
		t.Fatalf("create mandate: %v", err)
	}
	res, err := svc.GetMandate(ctx, tenantID, rec.IDRec())
	if err != nil {
		t.Fatalf("get mandate: %v", err)
	}
	if res.IDRec != rec.IDRec() {
		t.Fatalf("idRec: got %q want %q", res.IDRec, rec.IDRec())
	}
	// An idRec we never registered is not-found (tenant-scoped: no existence oracle).
	if _, err := svc.GetMandate(ctx, tenantID, "RN-nope"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown mandate: want ErrNotFound, got %v", err)
	}
	// Validation.
	if _, err := svc.GetMandate(ctx, "", "RN1"); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty tenant: want ErrValidation, got %v", err)
	}
	if _, err := svc.GetMandate(ctx, tenantID, "  "); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty idRec: want ErrValidation, got %v", err)
	}
	// Unavailable when the mandate port is not wired.
	if _, err := recurrenceNoBankHarness(t).GetMandate(ctx, "t1", "RN1"); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("nil port: want ErrUnavailable, got %v", err)
	}
}

func TestGetMandateQRWithExplicitTxID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _, tenantID := mandateJourneyHarness(t)
	loc, err := svc.CreateLocRec(ctx, app.CreateLocRecInput{TenantID: tenantID, IdempotencyKey: "loc-1"})
	if err != nil {
		t.Fatalf("create locrec: %v", err)
	}
	rec, _, err := svc.CreateMandate(ctx, mandateInput(tenantID, "tx-imediata", loc.ID, 9900))
	if err != nil {
		t.Fatalf("create mandate: %v", err)
	}
	// Explicit txID goes straight to the bank without consulting the durable binding.
	if _, err := svc.GetMandateQR(ctx, tenantID, rec.IDRec(), "tx-imediata"); err != nil {
		t.Fatalf("compose QR with explicit txid: %v", err)
	}
	// Validation + unavailable.
	if _, err := svc.GetMandateQR(ctx, "", rec.IDRec(), ""); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty tenant: want ErrValidation, got %v", err)
	}
	if _, err := svc.GetMandateQR(ctx, tenantID, "", ""); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty idRec: want ErrValidation, got %v", err)
	}
	if _, err := recurrenceNoBankHarness(t).GetMandateQR(ctx, "t1", "RN1", ""); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("nil port: want ErrUnavailable, got %v", err)
	}
}

func TestLocRecReadAndUnlink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _, tenantID := mandateJourneyHarness(t)
	loc, err := svc.CreateLocRec(ctx, app.CreateLocRecInput{TenantID: tenantID, IdempotencyKey: "loc-1"})
	if err != nil {
		t.Fatalf("create locrec: %v", err)
	}

	got, err := svc.GetLocRec(ctx, tenantID, loc.ID)
	if err != nil {
		t.Fatalf("get locrec: %v", err)
	}
	if got.ID != loc.ID {
		t.Fatalf("locrec id: got %d want %d", got.ID, loc.ID)
	}
	if _, err := svc.GetLocRec(ctx, tenantID, 999999); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown locrec: want ErrNotFound, got %v", err)
	}

	if _, err := svc.UnlinkLocRec(ctx, tenantID, loc.ID); err != nil {
		t.Fatalf("unlink locrec: %v", err)
	}

	// Validation.
	if _, err := svc.GetLocRec(ctx, "  ", loc.ID); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("get empty tenant: want ErrValidation, got %v", err)
	}
	if _, err := svc.UnlinkLocRec(ctx, "  ", loc.ID); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("unlink empty tenant: want ErrValidation, got %v", err)
	}
	// Unavailable.
	if _, err := recurrenceNoBankHarness(t).GetLocRec(ctx, "t1", 1); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("get nil port: want ErrUnavailable, got %v", err)
	}
	if _, err := recurrenceNoBankHarness(t).UnlinkLocRec(ctx, "t1", 1); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("unlink nil port: want ErrUnavailable, got %v", err)
	}
}

func TestCreateLocRecValidationAndUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := recurrenceNoBankHarness(t).CreateLocRec(ctx, app.CreateLocRecInput{TenantID: "  "}); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty tenant: want ErrValidation, got %v", err)
	}
	if _, err := recurrenceNoBankHarness(t).CreateLocRec(ctx, app.CreateLocRecInput{TenantID: "t1"}); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("nil port: want ErrUnavailable, got %v", err)
	}
}

func TestConfirmationReadAndValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _, tenantID := mandateJourneyHarness(t)
	rec, _, err := svc.CreateMandate(ctx, mandateInput(tenantID, "tx-1", 0, 0))
	if err != nil {
		t.Fatalf("create mandate: %v", err)
	}
	now := time.Unix(1000, 0).UTC() // the harness clock
	solic, err := svc.RequestConfirmation(ctx, app.RequestConfirmationInput{
		TenantID: tenantID, IDRec: rec.IDRec(), CPF: "02989131415",
		Agencia: "0001", Conta: "123456", ISPBParticipante: "12345678",
		ExpiraEm: now.Add(48 * time.Hour), IdempotencyKey: "solic-1",
	})
	if err != nil {
		t.Fatalf("request confirmation: %v", err)
	}
	got, err := svc.GetConfirmation(ctx, tenantID, solic.IDSolicRec)
	if err != nil {
		t.Fatalf("get confirmation: %v", err)
	}
	if got.IDRec != rec.IDRec() {
		t.Fatalf("solicrec idRec: got %q want %q", got.IDRec, rec.IDRec())
	}

	// Validation on both read and write boundaries.
	if _, err := svc.GetConfirmation(ctx, "", "SC1"); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("get empty tenant: want ErrValidation, got %v", err)
	}
	if _, err := svc.GetConfirmation(ctx, tenantID, "  "); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("get empty idSolicRec: want ErrValidation, got %v", err)
	}
	if _, err := svc.RequestConfirmation(ctx, app.RequestConfirmationInput{TenantID: tenantID, IDRec: "  "}); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("request empty idRec: want ErrValidation, got %v", err)
	}
	// Unavailable.
	if _, err := recurrenceNoBankHarness(t).GetConfirmation(ctx, "t1", "SC1"); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("get nil port: want ErrUnavailable, got %v", err)
	}
	if _, err := recurrenceNoBankHarness(t).RequestConfirmation(ctx, app.RequestConfirmationInput{TenantID: "t1", IDRec: "RN1"}); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("request nil port: want ErrUnavailable, got %v", err)
	}
}

func TestGetCobR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, h, tenantID := mandateJourneyHarness(t)
	approvedMandateWithCharge(t, svc, h, tenantID, "tx-1", "tx-cycle", 5000)

	res, err := svc.GetCobR(ctx, tenantID, "tx-cycle")
	if err != nil {
		t.Fatalf("get cobr: %v", err)
	}
	if res.TxID != "tx-cycle" {
		t.Fatalf("txid: got %q want %q", res.TxID, "tx-cycle")
	}
	if _, err := svc.GetCobR(ctx, tenantID, "tx-nope"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown charge: want ErrNotFound, got %v", err)
	}
	// Validation + unavailable.
	if _, err := svc.GetCobR(ctx, "", "tx"); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty tenant: want ErrValidation, got %v", err)
	}
	if _, err := svc.GetCobR(ctx, tenantID, "  "); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("empty txid: want ErrValidation, got %v", err)
	}
	if _, err := recurrenceNoBankHarness(t).GetCobR(ctx, "t1", "tx"); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("nil port: want ErrUnavailable, got %v", err)
	}
}

func TestRetryCobR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, h, tenantID := mandateJourneyHarness(t)
	idRec := approvedMandateWithCharge(t, svc, h, tenantID, "tx-1", "tx-cycle", 5000)

	// Happy path: an approved mandate may retry its own failed debit.
	if _, err := svc.RetryCobR(ctx, tenantID, idRec, "tx-cycle", "2026-09-15"); err != nil {
		t.Fatalf("retry cobr: %v", err)
	}

	// The mandate gate still applies: an unknown mandate cannot reach back and retry.
	if _, err := svc.RetryCobR(ctx, tenantID, "RN-nope", "tx-cycle", "2026-09-15"); !errors.Is(err, recurrence.ErrMandateNotFound) {
		t.Fatalf("unknown mandate: want ErrMandateNotFound, got %v", err)
	}

	// A mandate that is not APROVADA must not be able to retry a debit.
	rec2, _, err := svc.CreateMandate(ctx, mandateInput(tenantID, "tx-2", 0, 0))
	if err != nil {
		t.Fatalf("create second mandate: %v", err)
	}
	if _, err := svc.RetryCobR(ctx, tenantID, rec2.IDRec(), "tx-cycle", "2026-09-15"); !errors.Is(err, recurrence.ErrMandateNotApproved) {
		t.Fatalf("unapproved mandate: want ErrMandateNotApproved, got %v", err)
	}

	// Validation on every required field.
	for name, in := range map[string][4]string{
		"tenant": {"", idRec, "tx-cycle", "2026-09-15"},
		"idRec":  {tenantID, "", "tx-cycle", "2026-09-15"},
		"txid":   {tenantID, idRec, "", "2026-09-15"},
		"data":   {tenantID, idRec, "tx-cycle", ""},
	} {
		if _, err := svc.RetryCobR(ctx, in[0], in[1], in[2], in[3]); !errors.Is(err, shared.ErrValidation) {
			t.Fatalf("missing %s: want ErrValidation, got %v", name, err)
		}
	}
	// Unavailable.
	if _, err := recurrenceNoBankHarness(t).RetryCobR(ctx, "t1", "RN1", "tx", "2026-09-15"); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("nil port: want ErrUnavailable, got %v", err)
	}
}

// TestCreateMandateAcceptsCNPJPayer covers the CNPJ arm of toRecDevedor: a 14-digit
// document is mapped onto the BACEN oneOf as a CNPJ, not a CPF.
func TestCreateMandateAcceptsCNPJPayer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, h, tenantID := mandateJourneyHarness(t)
	in := mandateInput(tenantID, "tx-cnpj", 0, 0)
	in.DevedorDoc = "02989131415000" // 14 digits → CNPJ
	rec, _, err := svc.CreateMandate(ctx, in)
	if err != nil {
		t.Fatalf("create mandate with CNPJ payer: %v", err)
	}
	stored, err := h.store.FindRecByID(ctx, tenantID, rec.IDRec())
	if err != nil {
		t.Fatalf("reload mandate: %v", err)
	}
	if stored.Devedor().Doc() != "02989131415000" {
		t.Fatalf("payer doc not persisted: %q", stored.Devedor().Doc())
	}
}
