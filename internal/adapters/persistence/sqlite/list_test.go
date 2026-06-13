package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

func TestListTenantsAndProfileRoundTrip(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a, err := tenant.NewWithProfile("a", tenant.Profile{
		Name: "Acme LTDA", DisplayName: "Acme", CNPJ: "11.222.333/0001-81", Email: "ops@acme.com",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	b := tenant.RehydrateProfile("b", "Beta SA", "Beta", "", "fin@beta.com", true, t0.Add(time.Hour))
	if err := store.SaveTenant(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTenant(ctx, b); err != nil {
		t.Fatal(err)
	}

	// Profile fields round-trip through SQLite.
	got, err := store.FindTenantByID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.CNPJ() != "11222333000181" || got.Email() != "ops@acme.com" || got.DisplayName() != "Acme" {
		t.Fatalf("profile not persisted: %+v", got)
	}

	// ListTenants returns newest-first (Beta created later).
	list, err := store.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID() != "b" || list[1].ID() != "a" {
		t.Fatalf("unexpected order: %v", idsOf(list))
	}
}

func TestListTenantsEmpty(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	list, err := store.ListTenants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}
}

func idsOf(ts []*tenant.Tenant) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID()
	}
	return out
}
