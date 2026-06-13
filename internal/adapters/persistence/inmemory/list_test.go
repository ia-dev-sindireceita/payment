package inmemory_test

import (
	"context"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

func TestListTenantsOrdering(t *testing.T) {
	t.Parallel()
	store := inmemory.NewStore()
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Insert out of order; ListTenants must return newest-first.
	_ = store.SaveTenant(ctx, tenant.RehydrateProfile("old", "Old", "", "", "", true, t0))
	_ = store.SaveTenant(ctx, tenant.RehydrateProfile("new", "New", "", "", "", true, t0.Add(time.Hour)))
	// Same timestamp => deterministic tie-break by id desc.
	_ = store.SaveTenant(ctx, tenant.RehydrateProfile("zzz", "Z", "", "", "", true, t0.Add(time.Hour)))

	list, err := store.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(list))
	for i, tn := range list {
		got[i] = tn.ID()
	}
	want := []string{"zzz", "new", "old"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestListTenantsEmpty(t *testing.T) {
	t.Parallel()
	store := inmemory.NewStore()
	list, err := store.ListTenants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}
}
