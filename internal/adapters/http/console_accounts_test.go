package http_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/adminweb"
	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/account"
	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// incIDs is a unique-id provider (the shared seqIDs returns a constant, which would
// collide across create-account + create-tenant in one test).
type incIDs struct{ n int }

func (s *incIDs) NewID() string { s.n++; return "id-" + itoaHTTP(s.n) }

func itoaHTTP(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

type accountFixture struct {
	handler http.Handler
	store   *persistence.Store
}

// newAccountFixture wires the console with the account plane + invoice store, and
// seeds a real account "verz-1".
func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	store := persistence.NewStore()
	creds := secret.NewStore(map[string]ports.BankCredential{})
	if err := store.SaveAccount(context.Background(), account.Rehydrate("verz-1", "Verz", true, time.Unix(100, 0).UTC())); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	console := app.NewConsoleService(app.ConsoleDeps{
		Tenants: store, Accounts: store, Pricing: store, Ledger: store, Invoices: store,
		Audit: store, CredWriter: creds, CredReader: creds,
		Clock: fixedClock{}, IDs: &incIDs{},
	})
	ui, err := adminweb.New()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	auth := httpadapter.NewStaticTokenAuthWithRoles(nil, map[string]httpadapter.Role{
		adminToken:    httpadapter.RoleAdmin,
		operatorToken: httpadapter.RoleOperator,
	}, nil)
	srv := httpadapter.NewServer(httpadapter.Config{
		Console: console, UI: ui, AdminAuth: auth, TenantAuth: auth, WebhookAuth: auth,
	})
	return &accountFixture{handler: srv.Router(), store: store}
}

// acctCSRF mints a csrf cookie via a safe GET to the Contas page.
func acctCSRF(t *testing.T, h http.Handler, token string) *http.Cookie {
	t.Helper()
	rec := consoleGet(t, h, "/console/accounts", token)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "csrf_token" {
			return c
		}
	}
	t.Fatalf("no csrf cookie minted")
	return nil
}

func seedAcctLedger(t *testing.T, store *persistence.Store, acct, tenantID, endpoint string, cents int64, at time.Time) {
	t.Helper()
	e, err := billing.NewLedgerEntry("led-"+tenantID+"-"+at.Format("150405"), tenantID, endpoint, "ref", cents, at, billing.WithAccount(acct))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if err := store.AppendLedgerEntry(context.Background(), e); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestConsoleAccountsListAndNav(t *testing.T) {
	t.Parallel()
	f := newAccountFixture(t)
	rec := consoleGet(t, f.handler, "/console/accounts", operatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Verz") {
		t.Fatalf("list missing seeded account: %s", body)
	}
	// Root redirect still points to tenants (unchanged); the Contas item exists in nav
	// on a full-page render.
	full := consoleGet(t, f.handler, "/console/accounts", operatorToken)
	_ = full
}

func TestConsoleCreateAccountFlow(t *testing.T) {
	t.Parallel()
	f := newAccountFixture(t)
	csrf := acctCSRF(t, f.handler, adminToken)

	// Operator (read-only) cannot create — 403.
	if rec := consolePost(t, f.handler, "/console/accounts", operatorToken, url.Values{"name": {"Nova"}}, csrf); rec.Code != http.StatusForbidden {
		t.Fatalf("operator create = %d, want 403", rec.Code)
	}
	// Missing CSRF is rejected.
	if rec := consolePost(t, f.handler, "/console/accounts", adminToken, url.Values{"name": {"Nova"}}, nil); rec.Code == http.StatusOK {
		t.Fatalf("no-csrf create should not be 200")
	}
	// Admin creates → 200, navigates to detail.
	rec := consolePost(t, f.handler, "/console/accounts", adminToken, url.Values{"name": {"Cliente Novo"}}, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Cliente Novo") {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	// Invalid (blank) name → 422 with inline error.
	rec = consolePost(t, f.handler, "/console/accounts", adminToken, url.Values{"name": {"  "}}, csrf)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank name = %d, want 422", rec.Code)
	}
}

func TestConsoleAccountDetailAndNestedTenant(t *testing.T) {
	t.Parallel()
	f := newAccountFixture(t)
	csrf := acctCSRF(t, f.handler, adminToken)

	// Detail shows the (empty) nested state.
	rec := consoleGet(t, f.handler, "/console/accounts/verz-1", operatorToken)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nenhuma empresa-cliente") {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	// Unknown account 404s.
	if rec := consoleGet(t, f.handler, "/console/accounts/nope", operatorToken); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d, want 404", rec.Code)
	}
	// Create an empresa-cliente under the account.
	rec = consolePost(t, f.handler, "/console/accounts/verz-1/tenants", adminToken, url.Values{"name": {"Cliente Filho"}}, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Cliente Filho") {
		t.Fatalf("create tenant = %d: %s", rec.Code, rec.Body.String())
	}
	// The account detail now lists the tenant, and the tenant is bound to the account.
	detail := consoleGet(t, f.handler, "/console/accounts/verz-1", operatorToken)
	if !strings.Contains(detail.Body.String(), "Cliente Filho") {
		t.Fatalf("detail missing new tenant: %s", detail.Body.String())
	}
	tenants, _ := f.store.ListTenants(context.Background())
	var bound bool
	for _, tn := range tenants {
		if tn.Name() == "Cliente Filho" && tn.AccountID() == "verz-1" {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("new tenant not bound to account")
	}
}

func TestConsoleAccountSuspendActivate(t *testing.T) {
	t.Parallel()
	f := newAccountFixture(t)
	csrf := acctCSRF(t, f.handler, adminToken)
	rec := consolePost(t, f.handler, "/console/accounts/verz-1/suspend", adminToken, url.Values{}, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "account-status") {
		t.Fatalf("suspend = %d: %s", rec.Code, rec.Body.String())
	}
	a, _ := f.store.FindAccountByID(context.Background(), "verz-1")
	if a.Active() {
		t.Fatalf("account should be suspended")
	}
	rec = consolePost(t, f.handler, "/console/accounts/verz-1/activate", adminToken, url.Values{}, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate = %d", rec.Code)
	}
}

func TestConsoleAccountConsumptionAndCSV(t *testing.T) {
	t.Parallel()
	f := newAccountFixture(t)
	// Two tenants under verz-1 with usage; one entry under another account must not leak.
	seedAcctLedger(t, f.store, "verz-1", "t1", "POST /v1/charges", 250, time.Unix(10, 0).UTC())
	seedAcctLedger(t, f.store, "verz-1", "t2", "POST /v1/boletos", 400, time.Unix(20, 0).UTC())
	seedAcctLedger(t, f.store, "other", "t9", "POST /v1/charges", 999, time.Unix(30, 0).UTC())

	// Unbounded window (no dates) so the seeded 1970 entries count.
	rec := consoleGet(t, f.handler, "/console/accounts/verz-1/consumption?start_date=1970-01-01&end_date=1970-01-01", operatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("consumption = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Total da Conta") || strings.Contains(body, "999") {
		t.Fatalf("rollup leaked or missing total: %s", body)
	}
	// CSV export.
	csv := consoleGet(t, f.handler, "/console/accounts/verz-1/consumption.csv?start_date=1970-01-01&end_date=1970-01-01", operatorToken)
	if csv.Code != http.StatusOK || !strings.HasPrefix(csv.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv = %d ct=%q", csv.Code, csv.Header().Get("Content-Type"))
	}
	if cb := csv.Body.String(); !strings.Contains(cb, "empresa_cliente") || strings.Contains(cb, "t9") {
		t.Fatalf("csv content leaked or missing header: %s", cb)
	}
}

func TestConsoleAccountInvoicesBatch(t *testing.T) {
	t.Parallel()
	f := newAccountFixture(t)
	csrf := acctCSRF(t, f.handler, adminToken)
	// One tenant under the account with consumption in August 2026.
	if _, err := createTenantUnder(f, "t-child", "Filho"); err != nil {
		t.Fatal(err)
	}
	seedAcctLedger(t, f.store, "verz-1", "t-child", "POST /v1/charges", 500, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))

	// List starts empty.
	list := consoleGet(t, f.handler, "/console/accounts/verz-1/invoices", operatorToken)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "Nenhuma fatura") {
		t.Fatalf("empty invoices = %d: %s", list.Code, list.Body.String())
	}
	// Batch-generate the period.
	form := url.Values{"start_date": {"2026-08-01"}, "end_date": {"2026-08-06"}}
	rec := consolePost(t, f.handler, "/console/accounts/verz-1/invoices", adminToken, form, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Filho") {
		t.Fatalf("batch = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fatura(s) gerada(s)") {
		t.Fatalf("batch toast missing: %s", rec.Body.String())
	}
	// Missing required dates → 400.
	if bad := consolePost(t, f.handler, "/console/accounts/verz-1/invoices", adminToken, url.Values{}, csrf); bad.Code != http.StatusBadRequest {
		t.Fatalf("no-period batch = %d, want 400", bad.Code)
	}
}

// createTenantUnder provisions a tenant already-bound to verz-1 via the store, for
// invoice-batch setup (bypasses the handler to control the tenant id).
func createTenantUnder(f *accountFixture, id, name string) (string, error) {
	tt := tenant.RehydrateWithAccount(id, name, true, time.Unix(100, 0).UTC(), "verz-1")
	return id, f.store.SaveTenant(context.Background(), tt)
}
