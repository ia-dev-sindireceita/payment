package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

const validCNPJ = "11222333000181"

// consoleStepClock advances by a minute on each Now() so created_at ordering is
// deterministic and distinct. (fixedClock from app_test.go is single-valued.)
type consoleStepClock struct{ t time.Time }

func (c *consoleStepClock) Now() time.Time {
	c.t = c.t.Add(time.Minute)
	return c.t
}

func newConsole() *app.ConsoleService {
	store := inmemory.NewStore()
	return app.NewConsoleService(store, &consoleStepClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}, &seqIDs{})
}

func mustCreate(t *testing.T, svc *app.ConsoleService, name, cnpj, email string) *tenant.Tenant {
	t.Helper()
	tn, err := svc.CreateTenant(context.Background(), tenant.Profile{Name: name, CNPJ: cnpj, Email: email})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return tn
}

func TestConsoleCreateAndGet(t *testing.T) {
	t.Parallel()
	svc := newConsole()
	tn := mustCreate(t, svc, "Acme LTDA", validCNPJ, "ops@acme.com")

	got, err := svc.GetTenant(context.Background(), tn.ID())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name() != "Acme LTDA" || got.CNPJ() != validCNPJ {
		t.Fatalf("unexpected tenant %+v", got)
	}
}

func TestConsoleCreateValidationError(t *testing.T) {
	t.Parallel()
	svc := newConsole()
	_, err := svc.CreateTenant(context.Background(), tenant.Profile{Name: "X", CNPJ: "123", Email: "a@b.co"})
	var ve *shared.ValidationError
	if !errors.As(err, &ve) || ve.Field != "cnpj" {
		t.Fatalf("want cnpj validation error, got %v", err)
	}
}

func TestConsoleGetNotFound(t *testing.T) {
	t.Parallel()
	svc := newConsole()
	if _, err := svc.GetTenant(context.Background(), "missing"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestConsoleListFiltersAndOrder(t *testing.T) {
	t.Parallel()
	svc := newConsole()
	a := mustCreate(t, svc, "Acme Pagamentos", validCNPJ, "ops@acme.com")
	mustCreate(t, svc, "Beta Cobranças", "11444777000161", "fin@beta.com")
	// Suspend Acme to exercise the status filter.
	if _, err := svc.SuspendTenant(context.Background(), a.ID()); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// Newest-first: Beta (created later) before Acme.
	all, err := svc.ListTenants(context.Background(), app.ListTenantsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name() != "Beta Cobranças" {
		t.Fatalf("order wrong: %v", names(all))
	}

	// Search by substring (case-insensitive) over name.
	got, _ := svc.ListTenants(context.Background(), app.ListTenantsQuery{Search: "acme"})
	if len(got) != 1 || got[0].Name() != "Acme Pagamentos" {
		t.Fatalf("search failed: %v", names(got))
	}

	// Search by CNPJ digits.
	got, _ = svc.ListTenants(context.Background(), app.ListTenantsQuery{Search: "11444777"})
	if len(got) != 1 || got[0].Name() != "Beta Cobranças" {
		t.Fatalf("cnpj search failed: %v", names(got))
	}

	// Status filters.
	active, _ := svc.ListTenants(context.Background(), app.ListTenantsQuery{Status: app.StatusActive})
	if len(active) != 1 || active[0].Name() != "Beta Cobranças" {
		t.Fatalf("active filter failed: %v", names(active))
	}
	suspended, _ := svc.ListTenants(context.Background(), app.ListTenantsQuery{Status: app.StatusSuspended})
	if len(suspended) != 1 || suspended[0].Name() != "Acme Pagamentos" {
		t.Fatalf("suspended filter failed: %v", names(suspended))
	}

	// No match.
	none, _ := svc.ListTenants(context.Background(), app.ListTenantsQuery{Search: "zzz"})
	if len(none) != 0 {
		t.Fatalf("expected no matches, got %v", names(none))
	}
}

func TestConsoleSuspendActivate(t *testing.T) {
	t.Parallel()
	svc := newConsole()
	tn := mustCreate(t, svc, "Gama", validCNPJ, "g@x.co")

	s, err := svc.SuspendTenant(context.Background(), tn.ID())
	if err != nil || s.Active() {
		t.Fatalf("suspend: active=%v err=%v", s.Active(), err)
	}
	a, err := svc.ActivateTenant(context.Background(), tn.ID())
	if err != nil || !a.Active() {
		t.Fatalf("activate: active=%v err=%v", a.Active(), err)
	}
}

func TestConsoleTransitionNotFound(t *testing.T) {
	t.Parallel()
	svc := newConsole()
	if _, err := svc.SuspendTenant(context.Background(), "nope"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := svc.ActivateTenant(context.Background(), "nope"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestParseStatusFilter(t *testing.T) {
	t.Parallel()
	cases := map[string]app.StatusFilter{
		"":          app.StatusAny,
		"active":    app.StatusActive,
		"ACTIVE":    app.StatusActive,
		"suspended": app.StatusSuspended,
		"garbage":   app.StatusAny,
	}
	for in, want := range cases {
		if got := app.ParseStatusFilter(in); got != want {
			t.Fatalf("ParseStatusFilter(%q) = %q, want %q", in, got, want)
		}
	}
}

func names(ts []*tenant.Tenant) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
