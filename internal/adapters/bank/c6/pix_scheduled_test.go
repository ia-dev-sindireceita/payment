package c6

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// cobvTestServer is a configurable C6 cobv (scheduled charge) + OAuth2 double backed
// by httptest TLS. It stores charges by txid so idempotent re-submits can be asserted
// and records the last request for header/body checks.
type cobvTestServer struct {
	*httptest.Server

	mu          sync.Mutex
	cobs        map[string]pixChargeResponseBody // keyed by txid (PUT target)
	putHits     int
	lastMethod  string
	lastAuth    string
	lastIdemKey string
	lastReqBody pixCobvRequestBody
	listBody    pixListResponseBody

	putHandler http.HandlerFunc
	getHandler http.HandlerFunc
}

func newCobvTestServer(t *testing.T) *cobvTestServer {
	t.Helper()
	ts := &cobvTestServer{cobs: make(map[string]pixChargeResponseBody)}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-` + user + `","token_type":"Bearer","expires_in":3600}`))
	})
	// List: GET /v2/cobv (exact). Registered before the subtree so it is not shadowed.
	mux.HandleFunc("/v2/cobv", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.lastMethod = r.Method
		body := ts.listBody
		ts.mu.Unlock()
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/v2/cobv/", func(w http.ResponseWriter, r *http.Request) {
		txid := strings.TrimPrefix(r.URL.Path, "/v2/cobv/")
		ts.mu.Lock()
		ts.lastMethod = r.Method
		ts.lastAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodPut:
			ts.putHits++
			ts.lastIdemKey = r.Header.Get("Idempotency-Key")
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ts.lastReqBody)
			h := ts.putHandler
			ts.mu.Unlock()
			if h != nil {
				h(w, r)
				return
			}
			ts.mu.Lock()
			cob, ok := ts.cobs[txid]
			if !ok {
				cob = pixChargeResponseBody{
					TxID:          txid,
					Status:        "ATIVA",
					Valor:         pixValor{Original: ts.lastReqBody.Valor.Original},
					Loc:           pixLoc{Location: "pix.example.com/qr/cobv/" + txid},
					PixCopiaECola: "000201-cobv-" + txid,
				}
				ts.cobs[txid] = cob
			}
			ts.mu.Unlock()
			writeJSON(w, http.StatusCreated, cob)
		case http.MethodGet:
			h := ts.getHandler
			cob, ok := ts.cobs[txid]
			ts.mu.Unlock()
			if h != nil {
				h(w, r)
				return
			}
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
				return
			}
			writeJSON(w, http.StatusOK, cob)
		default:
			ts.mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ts.Server = httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (ts *cobvTestServer) provider(t *testing.T, creds ports.CredentialStore, now func() time.Time) *Provider {
	t.Helper()
	p, err := New(Config{
		BaseURL:    ts.URL,
		TokenURL:   ts.URL + "/oauth/token",
		HTTPClient: ts.Client(),
		Now:        now,
	}, creds)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func validCobvReq() ports.ScheduledChargeRequest {
	return ports.ScheduledChargeRequest{
		TenantID:             "t1",
		PaymentID:            "pay-1",
		IdempotencyKey:       "idem-1",
		AmountCents:          2500,
		Currency:             "BRL",
		DebtorTaxID:          "12345678901",
		DebtorName:           "Maria Silva",
		DueDate:              time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		ValidityAfterDueDays: 30,
	}
}

func TestCreateScheduledChargeSuccess(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	p := ts.provider(t, oneTenant("t1", "client-1", "secret-1"), nil)

	res, err := p.CreateScheduledCharge(context.Background(), "t1", validCobvReq())
	if err != nil {
		t.Fatalf("CreateScheduledCharge: %v", err)
	}
	if ts.lastMethod != http.MethodPut {
		t.Fatalf("expected PUT, got %s", ts.lastMethod)
	}
	if ts.lastAuth != "Bearer tok-client-1" {
		t.Fatalf("bearer token not attached: %q", ts.lastAuth)
	}
	if ts.lastIdemKey != "idem-1" {
		t.Fatalf("idempotency key not forwarded: %q", ts.lastIdemKey)
	}
	if ts.lastReqBody.Calendario.DataDeVencimento != "2026-07-15" {
		t.Fatalf("dataDeVencimento: want 2026-07-15, got %q", ts.lastReqBody.Calendario.DataDeVencimento)
	}
	if ts.lastReqBody.Calendario.ValidadeAposVencimento != 30 {
		t.Fatalf("validadeAposVencimento: want 30, got %d", ts.lastReqBody.Calendario.ValidadeAposVencimento)
	}
	if ts.lastReqBody.Valor.Original != "25.00" {
		t.Fatalf("valor.original: want 25.00, got %q", ts.lastReqBody.Valor.Original)
	}
	if ts.lastReqBody.Devedor == nil || ts.lastReqBody.Devedor.CPF != "12345678901" || ts.lastReqBody.Devedor.Nome != "Maria Silva" {
		t.Fatalf("devedor not mapped: %+v", ts.lastReqBody.Devedor)
	}
	wantTx := scheduledTxID("idem-1")
	if res.TxID != wantTx {
		t.Fatalf("txid: want %q, got %q", wantTx, res.TxID)
	}
	if res.Status != "ATIVA" || res.QRCodePayload == "" || res.QRCodeLocation == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.ExpectedAmountCents != 2500 {
		t.Fatalf("expected amount: %d", res.ExpectedAmountCents)
	}
}

// A CNPJ-length debtor document is placed in the cnpj field, not cpf.
func TestCreateScheduledChargeDevedorCNPJ(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
	req := validCobvReq()
	req.DebtorTaxID = "12345678000199"
	if _, err := p.CreateScheduledCharge(context.Background(), "t1", req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ts.lastReqBody.Devedor.CNPJ != "12345678000199" || ts.lastReqBody.Devedor.CPF != "" {
		t.Fatalf("CNPJ not mapped: %+v", ts.lastReqBody.Devedor)
	}
}

func TestCreateScheduledChargeBoundaryGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  ports.ScheduledChargeRequest
	}{
		{"empty anchor", func() ports.ScheduledChargeRequest {
			r := validCobvReq()
			r.IdempotencyKey = ""
			r.PaymentID = ""
			return r
		}()},
		{"zero amount", func() ports.ScheduledChargeRequest { r := validCobvReq(); r.AmountCents = 0; return r }()},
		{"negative amount", func() ports.ScheduledChargeRequest { r := validCobvReq(); r.AmountCents = -1; return r }()},
		{"zero due date", func() ports.ScheduledChargeRequest { r := validCobvReq(); r.DueDate = time.Time{}; return r }()},
		{"negative validity", func() ports.ScheduledChargeRequest { r := validCobvReq(); r.ValidityAfterDueDays = -1; return r }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newCobvTestServer(t)
			p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
			if _, err := p.CreateScheduledCharge(context.Background(), "t1", tc.req); !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
			ts.mu.Lock()
			hits := ts.putHits
			ts.mu.Unlock()
			if hits != 0 {
				t.Fatalf("PSP must not be touched on a boundary failure, putHits=%d", hits)
			}
		})
	}
}

func TestCreateScheduledChargeErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		want   error
	}{
		{422, shared.ErrValidation},
		{409, shared.ErrConflict},
		{503, shared.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			ts := newCobvTestServer(t)
			ts.putHandler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tc.status, map[string]string{"code": "X"})
			}
			p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
			if _, err := p.CreateScheduledCharge(context.Background(), "t1", validCobvReq()); !errors.Is(err, tc.want) {
				t.Fatalf("status %d: want %v, got %v", tc.status, tc.want, err)
			}
		})
	}
}

func TestGetScheduledChargeReconcile(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	ts.cobs["tx-paid"] = pixChargeResponseBody{
		TxID:   "tx-paid",
		Status: "CONCLUIDA",
		Valor:  pixValor{Original: "25.00"},
		Pix:    []pixReceipt{{Valor: "25.00"}},
	}
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)

	res, err := p.GetScheduledCharge(context.Background(), "t1", "tx-paid")
	if err != nil {
		t.Fatalf("GetScheduledCharge: %v", err)
	}
	if res.Status != "CONCLUIDA" || res.ExpectedAmountCents != 2500 || res.ReceivedAmountCents != 2500 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !res.AmountReconciled() {
		t.Fatalf("a fully-paid scheduled charge must reconcile: %+v", res)
	}
	if ts.lastMethod != http.MethodGet {
		t.Fatalf("expected GET, got %s", ts.lastMethod)
	}
}

func TestGetScheduledChargeNotFound(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
	if _, err := p.GetScheduledCharge(context.Background(), "t1", "missing"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListScheduledChargesSuccess(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	ts.listBody = pixListResponseBody{Cobs: []pixChargeResponseBody{
		{TxID: "tx-1", Status: "ATIVA", Valor: pixValor{Original: "10.00"}},
		{TxID: "tx-2", Status: "CONCLUIDA", Valor: pixValor{Original: "20.00"}, Pix: []pixReceipt{{Valor: "20.00"}}},
	}}
	ts.listBody.Parametros.Paginacao.PaginaAtual = 0
	ts.listBody.Parametros.Paginacao.ItensPorPagina = 100
	ts.listBody.Parametros.Paginacao.QuantidadeDePaginas = 1
	ts.listBody.Parametros.Paginacao.QuantidadeTotalDeItens = 2
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)

	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	list, err := p.ListScheduledCharges(context.Background(), "t1", ports.PixListFilter{
		Start: now.Add(-time.Hour), End: now.Add(time.Hour), Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("ListScheduledCharges: %v", err)
	}
	if list.TotalItems != 2 || len(list.Charges) != 2 {
		t.Fatalf("unexpected list: total=%d len=%d", list.TotalItems, len(list.Charges))
	}
	if list.Charges[1].ExpectedAmountCents != 2000 || !list.Charges[1].AmountReconciled() {
		t.Fatalf("reconcile in list failed: %+v", list.Charges[1])
	}
}

func TestListScheduledChargesMissingWindow(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
	if _, err := p.ListScheduledCharges(context.Background(), "t1", ports.PixListFilter{}); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("want ErrValidation for empty window, got %v", err)
	}
}

func TestListScheduledChargesMalformedAmount(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	ts.listBody = pixListResponseBody{Cobs: []pixChargeResponseBody{
		{TxID: "tx-bad", Status: "CONCLUIDA", Valor: pixValor{Original: "abc"}},
	}}
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	if _, err := p.ListScheduledCharges(context.Background(), "t1", ports.PixListFilter{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("malformed amount in list should map to ErrUnavailable, got %v", err)
	}
}

func TestScheduledMissingCredential(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	p := ts.provider(t, &fakeCreds{creds: map[string]ports.BankCredential{}}, nil)
	if _, err := p.GetScheduledCharge(context.Background(), "unknown", "tx"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("missing credential should propagate ErrNotFound, got %v", err)
	}
}

func TestScheduledTxIDValid(t *testing.T) {
	t.Parallel()
	tx := scheduledTxID("idem-1")
	if len(tx) != pixTxIDLen || len(tx) < 26 || len(tx) > 35 {
		t.Fatalf("txid out of BACEN range: %q (len %d)", tx, len(tx))
	}
	if scheduledTxID("idem-1") != tx {
		t.Fatal("txid not deterministic")
	}
	if scheduledTxID("idem-2") == tx {
		t.Fatal("txid collided across distinct anchors")
	}
}

func TestScheduledAnchorFallsBackToPaymentID(t *testing.T) {
	t.Parallel()
	ts := newCobvTestServer(t)
	p := ts.provider(t, oneTenant("t1", "c", "s"), nil)
	req := validCobvReq()
	req.IdempotencyKey = ""
	if _, err := p.CreateScheduledCharge(context.Background(), "t1", req); err != nil {
		t.Fatalf("create: %v", err)
	}
	// No idempotency key => anchor (and forwarded Idempotency-Key) falls back to PaymentID.
	if ts.lastIdemKey != "pay-1" {
		t.Fatalf("idempotency key should fall back to payment id, got %q", ts.lastIdemKey)
	}
}
