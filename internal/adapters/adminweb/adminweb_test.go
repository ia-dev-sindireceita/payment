package adminweb_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/adminweb"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

const (
	csrfToken = "testtoken"
	validCNPJ = "11222333000181"
)

type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

type seqIDs struct{ n int }

func (s *seqIDs) NewID() string { s.n++; return fmt.Sprintf("t%d", s.n) }

func newFixture(t *testing.T) (http.Handler, *app.ConsoleService) {
	t.Helper()
	store := inmemory.NewStore()
	svc := app.NewConsoleService(store, &stepClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}, &seqIDs{})
	h, err := adminweb.New(svc, adminweb.Options{CSRFToken: func() string { return csrfToken }})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	mux := chi.NewRouter()
	mux.Mount("/console", h.Routes())
	return mux, svc
}

type reqOpts struct {
	hx      bool
	form    string // urlencoded body for POSTs
	noCSRF  bool
	headers map[string]string
}

func do(h http.Handler, method, target string, o reqOpts) *httptest.ResponseRecorder {
	var body io.Reader
	if o.form != "" {
		body = strings.NewReader(o.form)
	}
	req := httptest.NewRequest(method, target, body)
	if o.form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if o.hx {
		req.Header.Set("HX-Request", "true")
	}
	if method != http.MethodGet && !o.noCSRF {
		req.AddCookie(&http.Cookie{Name: "pay_csrf", Value: csrfToken})
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func seed(t *testing.T, svc *app.ConsoleService, name, cnpj, email string) *tenant.Tenant {
	t.Helper()
	tn, err := svc.CreateTenant(context.Background(), tenant.Profile{Name: name, CNPJ: cnpj, Email: email})
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return tn
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("expected body to contain %q\n---\n%s", want, body)
	}
}

func mustNotContain(t *testing.T, body, want string) {
	t.Helper()
	if strings.Contains(body, want) {
		t.Fatalf("expected body NOT to contain %q\n---\n%s", want, body)
	}
}

func TestListEmptyState(t *testing.T) {
	h, _ := newFixture(t)
	rec := do(h, http.MethodGet, "/console/tenants", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "<html")
	mustContain(t, body, "Nenhum tenant ainda")
	// Security headers applied.
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP header")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing X-Frame-Options")
	}
}

func TestListWithTenants(t *testing.T) {
	h, svc := newFixture(t)
	seed(t, svc, "Acme Pagamentos", validCNPJ, "ops@acme.com")
	rec := do(h, http.MethodGet, "/console/tenants", reqOpts{})
	body := rec.Body.String()
	mustContain(t, body, "Acme Pagamentos")
	mustContain(t, body, "11.222.333/0001-81")
	mustContain(t, body, "Ativo")
}

func TestListSearchNoResult(t *testing.T) {
	h, svc := newFixture(t)
	seed(t, svc, "Acme", validCNPJ, "a@b.co")
	rec := do(h, http.MethodGet, "/console/tenants?q=zzz", reqOpts{})
	mustContain(t, rec.Body.String(), "Nada encontrado")
}

func TestRowsFragmentIsPartial(t *testing.T) {
	h, svc := newFixture(t)
	seed(t, svc, "Acme", validCNPJ, "a@b.co")
	rec := do(h, http.MethodGet, "/console/tenants/rows?q=acme", reqOpts{hx: true})
	body := rec.Body.String()
	mustNotContain(t, body, "<html")
	mustContain(t, body, "Acme")
}

func TestNewFormHasCSRF(t *testing.T) {
	h, _ := newFixture(t)
	rec := do(h, http.MethodGet, "/console/tenants/new", reqOpts{})
	body := rec.Body.String()
	mustContain(t, body, `name="csrf_token"`)
	mustContain(t, body, csrfToken)
	mustContain(t, body, "Razão social")
}

func TestCreateSuccessNoJS(t *testing.T) {
	h, _ := newFixture(t)
	form := "name=Acme+LTDA&cnpj=" + validCNPJ + "&email=ops%40acme.com"
	rec := do(h, http.MethodPost, "/console/tenants", reqOpts{form: form})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/console/tenants/") {
		t.Fatalf("redirect = %q", loc)
	}
}

func TestCreateSuccessHX(t *testing.T) {
	h, _ := newFixture(t)
	form := "name=Acme+LTDA&cnpj=" + validCNPJ + "&email=ops%40acme.com"
	rec := do(h, http.MethodPost, "/console/tenants", reqOpts{form: form, hx: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if rec.Header().Get("HX-Push-Url") == "" {
		t.Fatal("missing HX-Push-Url")
	}
	body := rec.Body.String()
	mustContain(t, body, "Tenant criado")
	mustContain(t, body, "Acme LTDA")
	mustContain(t, body, `hx-swap-oob`)
}

func TestCreateValidationErrorHX(t *testing.T) {
	h, _ := newFixture(t)
	form := "name=Acme&cnpj=123&email=ops%40acme.com"
	rec := do(h, http.MethodPost, "/console/tenants", reqOpts{form: form, hx: true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "CNPJ deve ter 14 dígitos")
	mustContain(t, body, "has-error")
	mustContain(t, body, "autofocus")
	// Echoes the submitted name so the user does not retype it.
	mustContain(t, body, `value="Acme"`)
}

func TestCreateRejectedWithoutCSRF(t *testing.T) {
	h, _ := newFixture(t)
	form := "name=Acme+LTDA&cnpj=" + validCNPJ + "&email=ops%40acme.com"
	rec := do(h, http.MethodPost, "/console/tenants", reqOpts{form: form, noCSRF: true})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}

func TestCreateRejectedWithMismatchedCSRF(t *testing.T) {
	h, _ := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/console/tenants",
		strings.NewReader("name=A&cnpj="+validCNPJ+"&email=a%40b.co"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "pay_csrf", Value: csrfToken})
	req.Header.Set("X-CSRF-Token", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}

func TestDetailAndSuspendActivate(t *testing.T) {
	h, svc := newFixture(t)
	tn := seed(t, svc, "Acme", validCNPJ, "a@b.co")

	rec := do(h, http.MethodGet, "/console/tenants/"+tn.ID(), reqOpts{})
	body := rec.Body.String()
	mustContain(t, body, "Suspender")
	mustContain(t, body, "Visão geral")

	// Suspend via HX → OOB status + toast.
	rec = do(h, http.MethodPost, "/console/tenants/"+tn.ID()+"/suspend", reqOpts{hx: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend code = %d", rec.Code)
	}
	body = rec.Body.String()
	mustContain(t, body, "Suspenso")
	mustContain(t, body, "tenant-status")
	mustContain(t, body, "Tenant suspenso")

	// Activate via no-JS → redirect.
	rec = do(h, http.MethodPost, "/console/tenants/"+tn.ID()+"/activate", reqOpts{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("activate code = %d, want 303", rec.Code)
	}

	// Detail now shows Reativar (suspended) — wait, we just reactivated, so it
	// should show Suspender again.
	rec = do(h, http.MethodGet, "/console/tenants/"+tn.ID(), reqOpts{})
	mustContain(t, rec.Body.String(), "Suspender")
}

func TestDetailNotFound(t *testing.T) {
	h, _ := newFixture(t)
	rec := do(h, http.MethodGet, "/console/tenants/missing", reqOpts{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), "não encontrado")
}

func TestSuspendNotFound(t *testing.T) {
	h, _ := newFixture(t)
	rec := do(h, http.MethodPost, "/console/tenants/missing/suspend", reqOpts{hx: true})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestStaticAssets(t *testing.T) {
	h, _ := newFixture(t)
	for _, tc := range []struct{ path, ctype, marker string }{
		{"/console/static/tokens.css", "text/css", "--color-primary"},
		{"/console/static/app.css", "text/css", ".btn"},
		{"/console/static/htmx.min.js", "application/javascript", "htmx"},
	} {
		rec := do(h, http.MethodGet, tc.path, reqOpts{})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code = %d", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.ctype) {
			t.Fatalf("%s content-type = %q", tc.path, ct)
		}
		mustContain(t, rec.Body.String(), tc.marker)
	}
}

func TestStaticRejectsUnknown(t *testing.T) {
	h, _ := newFixture(t)
	for _, p := range []string{"/console/static/evil.css", "/console/static/../adminweb.go"} {
		rec := do(h, http.MethodGet, p, reqOpts{})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s code = %d, want 404", p, rec.Code)
		}
	}
}

func TestRootRedirect(t *testing.T) {
	h, _ := newFixture(t)
	rec := do(h, http.MethodGet, "/console/", reqOpts{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/console/tenants" {
		t.Fatalf("code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestDeferredStubs(t *testing.T) {
	h, svc := newFixture(t)
	tn := seed(t, svc, "Acme", validCNPJ, "a@b.co")

	for _, p := range []string{
		"/console/pricing",
		"/console/billing",
		"/console/tenants/" + tn.ID() + "/credentials",
		"/console/tenants/" + tn.ID() + "/pricing",
		"/console/tenants/" + tn.ID() + "/consumption",
	} {
		rec := do(h, http.MethodGet, p, reqOpts{})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code = %d", p, rec.Code)
		}
		mustContain(t, rec.Body.String(), "Em breve")
	}

	// Tenant-scoped stub for a missing tenant → 404.
	rec := do(h, http.MethodGet, "/console/tenants/missing/credentials", reqOpts{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing credentials code = %d, want 404", rec.Code)
	}
}

func TestCSRFCookieIssuedOnGet(t *testing.T) {
	h, _ := newFixture(t)
	rec := do(h, http.MethodGet, "/console/tenants", reqOpts{})
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "pay_csrf" {
			found = true
			if !c.HttpOnly {
				t.Fatal("csrf cookie must be HttpOnly")
			}
		}
	}
	if !found {
		t.Fatal("expected pay_csrf cookie to be set")
	}
}

func TestAuthMiddlewareApplied(t *testing.T) {
	store := inmemory.NewStore()
	svc := app.NewConsoleService(store, &stepClock{t: time.Now()}, &seqIDs{})
	denied := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		})
	}
	h, err := adminweb.New(svc, adminweb.Options{CSRFToken: func() string { return csrfToken }, Auth: denied})
	if err != nil {
		t.Fatal(err)
	}
	mux := chi.NewRouter()
	mux.Mount("/console", h.Routes())
	rec := do(mux, http.MethodGet, "/console/tenants", reqOpts{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("auth gate not applied: code = %d", rec.Code)
	}
	// Static assets remain reachable (no auth) for unauthenticated login pages.
	rec = do(mux, http.MethodGet, "/console/static/tokens.css", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("static should bypass auth: code = %d", rec.Code)
	}
}
