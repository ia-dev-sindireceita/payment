package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// TenantStore is the narrow output port the admin console needs over tenants:
// the foundation's TenantRepository plus listing. Concrete stores (sqlite,
// inmemory) satisfy it; the console depends only on this segregated interface.
type TenantStore interface {
	SaveTenant(ctx context.Context, t *tenant.Tenant) error
	FindTenantByID(ctx context.Context, id string) (*tenant.Tenant, error)
	ListTenants(ctx context.Context) ([]*tenant.Tenant, error)
}

// ConsoleService implements the human admin console use-cases (server-rendered
// HTMX UI): tenant lifecycle and listing. It is separate from AdminService (the
// programmatic JSON admin API) so each plane evolves independently. All
// operations are privileged; RBAC is enforced at the HTTP boundary.
type ConsoleService struct {
	tenants TenantStore
	clock   ports.Clock
	ids     ports.IDProvider
}

// NewConsoleService wires a ConsoleService from its dependencies.
func NewConsoleService(tenants TenantStore, clock ports.Clock, ids ports.IDProvider) *ConsoleService {
	return &ConsoleService{tenants: tenants, clock: clock, ids: ids}
}

// StatusFilter narrows a tenant listing by lifecycle status.
type StatusFilter string

const (
	// StatusAny matches active and suspended tenants.
	StatusAny StatusFilter = ""
	// StatusActive matches only active tenants.
	StatusActive StatusFilter = "active"
	// StatusSuspended matches only suspended tenants.
	StatusSuspended StatusFilter = "suspended"
)

// ParseStatusFilter maps a raw query value to a StatusFilter, defaulting to
// StatusAny for unknown/empty input (forgiving input handling).
func ParseStatusFilter(raw string) StatusFilter {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active":
		return StatusActive
	case "suspended":
		return StatusSuspended
	default:
		return StatusAny
	}
}

// ListTenantsQuery describes a filtered tenant listing.
type ListTenantsQuery struct {
	Search string       // case-insensitive substring over name/displayName/cnpj/email
	Status StatusFilter // lifecycle filter
}

// ListTenants returns tenants matching the query, newest-first.
func (s *ConsoleService) ListTenants(ctx context.Context, q ListTenantsQuery) ([]*tenant.Tenant, error) {
	all, err := s.tenants.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	out := make([]*tenant.Tenant, 0, len(all))
	for _, t := range all {
		if !matchesStatus(t, q.Status) {
			continue
		}
		if needle != "" && !matchesSearch(t, needle) {
			continue
		}
		out = append(out, t)
	}
	// ListTenants already returns newest-first, but enforce determinism here too
	// in case a store returns an unordered slice.
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := out[i].CreatedAt(), out[j].CreatedAt()
		if ci.Equal(cj) {
			return out[i].ID() > out[j].ID()
		}
		return ci.After(cj)
	})
	return out, nil
}

func matchesStatus(t *tenant.Tenant, f StatusFilter) bool {
	switch f {
	case StatusActive:
		return t.Active()
	case StatusSuspended:
		return !t.Active()
	default:
		return true
	}
}

func matchesSearch(t *tenant.Tenant, needle string) bool {
	for _, field := range []string{t.Name(), t.DisplayName(), t.CNPJ(), t.Email()} {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

// GetTenant returns a tenant by id, or shared.ErrNotFound.
func (s *ConsoleService) GetTenant(ctx context.Context, id string) (*tenant.Tenant, error) {
	t, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	return t, nil
}

// CreateTenant provisions a new tenant from the console form profile.
func (s *ConsoleService) CreateTenant(ctx context.Context, p tenant.Profile) (*tenant.Tenant, error) {
	t, err := tenant.NewWithProfile(s.ids.NewID(), p, s.clock.Now())
	if err != nil {
		return nil, err // already a domain validation error; surfaced inline by the boundary
	}
	if err := s.tenants.SaveTenant(ctx, t); err != nil {
		return nil, fmt.Errorf("save tenant: %w", err)
	}
	return t, nil
}

// SuspendTenant deactivates a tenant (reversible). Returns the updated tenant.
func (s *ConsoleService) SuspendTenant(ctx context.Context, id string) (*tenant.Tenant, error) {
	return s.transition(ctx, id, (*tenant.Tenant).Deactivate)
}

// ActivateTenant re-enables a suspended tenant. Returns the updated tenant.
func (s *ConsoleService) ActivateTenant(ctx context.Context, id string) (*tenant.Tenant, error) {
	return s.transition(ctx, id, (*tenant.Tenant).Activate)
}

func (s *ConsoleService) transition(ctx context.Context, id string, apply func(*tenant.Tenant)) (*tenant.Tenant, error) {
	t, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	apply(t)
	if err := s.tenants.SaveTenant(ctx, t); err != nil {
		return nil, fmt.Errorf("save tenant: %w", err)
	}
	return t, nil
}
