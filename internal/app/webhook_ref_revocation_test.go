package app_test

import (
	"context"
	"errors"
	"testing"

	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/domain/webhookref"
)

// seedTenantRef mints a ref for tenantID in the store (returns nothing — the plaintext
// is not needed; the test asserts the count).
func seedTenantRef(t *testing.T, store *persistence.WebhookRefStore, tenantID string) {
	t.Helper()
	ref, err := webhookref.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	sum := webhookref.Sum(ref)
	if err := store.PutWebhookRef(context.Background(), sum[:], tenantID); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestWebhookRefRevocationService(t *testing.T) {
	t.Parallel()

	t.Run("revokes active refs and reports the count", func(t *testing.T) {
		t.Parallel()
		store := persistence.NewWebhookRefStore()
		seedTenantRef(t, store, "emp-1")
		svc := app.NewWebhookRefRevocationService(store)

		n, err := svc.RevokeTenantRefs(context.Background(), "emp-1")
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if n != 1 {
			t.Fatalf("revoked = %d, want 1", n)
		}
		// A second revoke is idempotent: nothing left to revoke.
		if n2, err := svc.RevokeTenantRefs(context.Background(), "emp-1"); err != nil || n2 != 0 {
			t.Fatalf("idempotent revoke = (%d, %v), want (0, nil)", n2, err)
		}
	})

	t.Run("idempotent for a tenant with no ref", func(t *testing.T) {
		t.Parallel()
		svc := app.NewWebhookRefRevocationService(persistence.NewWebhookRefStore())
		n, err := svc.RevokeTenantRefs(context.Background(), "emp-none")
		if err != nil || n != 0 {
			t.Fatalf("revoke = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("blank tenant is a validation error", func(t *testing.T) {
		t.Parallel()
		svc := app.NewWebhookRefRevocationService(persistence.NewWebhookRefStore())
		if _, err := svc.RevokeTenantRefs(context.Background(), "   "); !errors.Is(err, shared.ErrValidation) {
			t.Fatalf("blank tenant err = %v, want validation", err)
		}
	})

	t.Run("nil store fails closed", func(t *testing.T) {
		t.Parallel()
		svc := app.NewWebhookRefRevocationService(nil)
		if _, err := svc.RevokeTenantRefs(context.Background(), "emp-1"); !errors.Is(err, app.ErrWebhookRefRevocationUnavailable) {
			t.Fatalf("nil-store err = %v, want ErrWebhookRefRevocationUnavailable", err)
		}
	})
}
