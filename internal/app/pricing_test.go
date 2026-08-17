package app

import (
	"context"
	"errors"
	"testing"

	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// stubPricing is a minimal ports.PricingRepository for exercising the
// resolvePriceOrFree policy in isolation (white-box). It never touches storage.
type stubPricing struct {
	price billing.EndpointPricing
	err   error
}

func (s stubPricing) GetEndpointPrice(context.Context, string, string) (billing.EndpointPricing, error) {
	return s.price, s.err
}

func (s stubPricing) UpsertEndpointPrice(context.Context, billing.EndpointPricing) error { return nil }

var errInfra = errors.New("db down")

func TestResolvePriceOrFree(t *testing.T) {
	t.Parallel()

	priced, _ := billing.NewEndpointPricing("t1", "pix.create", 99)

	tests := []struct {
		name       string
		repo       stubPricing
		wantCents  int64
		wantErr    error // errors.Is target; nil means no error expected
		wantTenant string
	}{
		{
			name:       "configured price is returned unchanged",
			repo:       stubPricing{price: priced},
			wantCents:  99,
			wantTenant: "t1",
		},
		{
			name:       "unpriced endpoint (ErrNotFound) becomes free (0), same tenant+endpoint",
			repo:       stubPricing{err: shared.ErrNotFound},
			wantCents:  0,
			wantTenant: "t1",
		},
		{
			name:    "infra error propagates — never masked as free (fail-safe)",
			repo:    stubPricing{err: errInfra},
			wantErr: errInfra,
		},
		{
			name:    "wrapped infra error still propagates",
			repo:    stubPricing{err: errWrap(errInfra)},
			wantErr: errInfra,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolvePriceOrFree(context.Background(), tt.repo, "t1", "pix.create")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v; want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.PriceCents() != tt.wantCents {
				t.Fatalf("price = %d; want %d", got.PriceCents(), tt.wantCents)
			}
			if got.TenantID() != tt.wantTenant || got.Endpoint() != "pix.create" {
				t.Fatalf("isolation: got (%q,%q); want (%q,%q)", got.TenantID(), got.Endpoint(), tt.wantTenant, "pix.create")
			}
		})
	}
}

// errWrap returns err wrapped so errors.Is still finds the cause (guards the
// unpriced-vs-infra distinction against wrapping in the adapter chain).
func errWrap(err error) error { return errJoin(err) }

func errJoin(err error) error { return errors.Join(errors.New("adapter: query failed"), err) }
