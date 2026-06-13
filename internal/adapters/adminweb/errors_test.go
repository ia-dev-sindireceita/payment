package adminweb_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/adminweb"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

// erroringConsole returns a generic (non-validation) error from every method, to
// exercise the adapter's 5xx / renderError paths.
type erroringConsole struct{}

var errBoom = errors.New("boom")

func (erroringConsole) ListTenants(context.Context, app.ListTenantsQuery) ([]*tenant.Tenant, error) {
	return nil, errBoom
}
func (erroringConsole) GetTenant(context.Context, string) (*tenant.Tenant, error) {
	return nil, errBoom
}
func (erroringConsole) CreateTenant(context.Context, tenant.Profile) (*tenant.Tenant, error) {
	return nil, errBoom
}
func (erroringConsole) SuspendTenant(context.Context, string) (*tenant.Tenant, error) {
	return nil, errBoom
}
func (erroringConsole) ActivateTenant(context.Context, string) (*tenant.Tenant, error) {
	return nil, errBoom
}

func newErroringFixture(t *testing.T) http.Handler {
	t.Helper()
	h, err := adminweb.New(erroringConsole{}, adminweb.Options{CSRFToken: func() string { return csrfToken }})
	if err != nil {
		t.Fatal(err)
	}
	mux := chi.NewRouter()
	mux.Mount("/console", h.Routes())
	return mux
}

func TestErrorPaths(t *testing.T) {
	h := newErroringFixture(t)

	// list + rows surface a 500 page.
	if rec := do(h, http.MethodGet, "/console/tenants", reqOpts{}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("list code = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/console/tenants/rows", reqOpts{hx: true}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("rows code = %d", rec.Code)
	}

	// create with a generic store error → 500 (not 422).
	form := "name=Acme+LTDA&cnpj=" + validCNPJ + "&email=ops%40acme.com"
	rec := do(h, http.MethodPost, "/console/tenants", reqOpts{form: form})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create code = %d, want 500", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Algo deu errado")

	// suspend on an erroring console → 500 (generic, not 404).
	if rec := do(h, http.MethodPost, "/console/tenants/x/suspend", reqOpts{hx: true}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("suspend code = %d", rec.Code)
	}
}

func TestCreateValidationFieldVariants(t *testing.T) {
	h, _ := newFixture(t)
	cases := []struct {
		name string
		form string
		want string // expected error substring + field marker id
		eid  string
	}{
		{"empty name", "name=&cnpj=" + validCNPJ + "&email=a%40b.co", "razão social", "e-name"},
		{"bad email", "name=Acme&cnpj=" + validCNPJ + "&email=nope", "mail", "e-email"},
		{"long display", "name=Acme&display_name=" + longField(201) + "&cnpj=" + validCNPJ + "&email=a%40b.co", "exibição", "e-display"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, http.MethodPost, "/console/tenants", reqOpts{form: tc.form, hx: true})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code = %d, want 422", rec.Code)
			}
			body := rec.Body.String()
			mustContain(t, body, tc.eid)
		})
	}
}

func longField(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
