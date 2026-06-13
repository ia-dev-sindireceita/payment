package app

import (
	"context"
	"fmt"

	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// AdminService implements the admin plane: tenant lifecycle and per-endpoint
// pricing. These operations are privileged (RBAC enforced at the boundary).
type AdminService struct {
	tenants ports.TenantRepository
	pricing ports.PricingRepository
	clock   ports.Clock
	ids     ports.IDProvider
}

// NewAdminService wires an AdminService from the provided ports.
func NewAdminService(d Deps) *AdminService {
	return &AdminService{tenants: d.Tenants, pricing: d.Pricing, clock: d.Clock, ids: d.IDs}
}

// CreateTenant provisions a new tenant and returns it.
func (s *AdminService) CreateTenant(ctx context.Context, name string) (*tenant.Tenant, error) {
	t, err := tenant.New(s.ids.NewID(), name, s.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := s.tenants.SaveTenant(ctx, t); err != nil {
		return nil, fmt.Errorf("save tenant: %w", err)
	}
	return t, nil
}

// SetEndpointPrice sets the price (cents) a tenant pays for an endpoint call.
// The tenant must exist. Tenants are read-only over their own pricing (threat B3).
func (s *AdminService) SetEndpointPrice(ctx context.Context, tenantID, endpoint string, priceCents int64) (billing.EndpointPricing, error) {
	if _, err := s.tenants.FindTenantByID(ctx, tenantID); err != nil {
		return billing.EndpointPricing{}, fmt.Errorf("resolve tenant: %w", err)
	}
	p, err := billing.NewEndpointPricing(tenantID, endpoint, priceCents)
	if err != nil {
		return billing.EndpointPricing{}, err
	}
	if err := s.pricing.UpsertEndpointPrice(ctx, p); err != nil {
		return billing.EndpointPricing{}, fmt.Errorf("upsert price: %w", err)
	}
	return p, nil
}
