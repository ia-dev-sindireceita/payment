// Package inmemory is an in-memory implementation of the repository ports. It
// exists to (a) demonstrate adapter plugability — swapping it for the SQLite
// adapter is pure wiring in cmd, with no change to the domain or use-cases — and
// (b) provide a fast, production-faithful store for tests (it enforces the same
// tenant scoping and idempotency invariants as the SQLite adapter).
package inmemory

import (
	"context"
	"sort"
	"sync"

	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/payment"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// Store is a concurrency-safe in-memory repository.
type Store struct {
	mu        sync.RWMutex
	tenants   map[string]*tenant.Tenant
	payments  map[string]*payment.Payment        // keyed by tenantID+"\x00"+id
	pricing   map[string]billing.EndpointPricing // keyed by tenantID+"\x00"+endpoint
	ledger    []billing.LedgerEntry
	processed map[string]struct{} // keyed by tenantID+"\x00"+eventKey
}

// NewStore returns an empty in-memory store.
func NewStore() *Store {
	return &Store{
		tenants:   make(map[string]*tenant.Tenant),
		payments:  make(map[string]*payment.Payment),
		pricing:   make(map[string]billing.EndpointPricing),
		processed: make(map[string]struct{}),
	}
}

var (
	_ ports.PaymentRepository   = (*Store)(nil)
	_ ports.TenantRepository    = (*Store)(nil)
	_ ports.PricingRepository   = (*Store)(nil)
	_ ports.LedgerRepository    = (*Store)(nil)
	_ ports.ProcessedEventStore = (*Store)(nil)
)

func key(a, b string) string { return a + "\x00" + b }

// SaveTenant stores a tenant.
func (s *Store) SaveTenant(ctx context.Context, t *tenant.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants[t.ID()] = t
	return nil
}

// FindTenantByID returns a tenant or ErrNotFound.
func (s *Store) FindTenantByID(ctx context.Context, id string) (*tenant.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tenants[id]
	if !ok {
		return nil, shared.ErrNotFound
	}
	return t, nil
}

// ListTenants returns all tenants ordered newest-first (by creation time, then
// id) to match the admin console layout. It returns a fresh slice each call.
func (s *Store) ListTenants(ctx context.Context) ([]*tenant.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*tenant.Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := out[i].CreatedAt(), out[j].CreatedAt()
		if ci.Equal(cj) {
			return out[i].ID() > out[j].ID()
		}
		return ci.After(cj)
	})
	return out, nil
}

// SavePayment stores a payment scoped by tenant.
func (s *Store) SavePayment(ctx context.Context, p *payment.Payment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payments[key(p.TenantID(), p.ID())] = p
	return nil
}

// FindPaymentByID returns a tenant-scoped payment or ErrNotFound.
func (s *Store) FindPaymentByID(ctx context.Context, tenantID, id string) (*payment.Payment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.payments[key(tenantID, id)]
	if !ok {
		return nil, shared.ErrNotFound
	}
	return p, nil
}

// FindPaymentByIdempotencyKey scans the tenant's payments for the key.
func (s *Store) FindPaymentByIdempotencyKey(ctx context.Context, tenantID, idemKey string) (*payment.Payment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.payments {
		if p.TenantID() == tenantID && p.IdempotencyKey() == idemKey {
			return p, nil
		}
	}
	return nil, shared.ErrNotFound
}

// FindPaymentByTxID scans the tenant's payments for the tx id.
func (s *Store) FindPaymentByTxID(ctx context.Context, tenantID, txID string) (*payment.Payment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.payments {
		if p.TenantID() == tenantID && p.TxID() == txID {
			return p, nil
		}
	}
	return nil, shared.ErrNotFound
}

// GetEndpointPrice returns the price for a tenant × endpoint or ErrNotFound.
func (s *Store) GetEndpointPrice(ctx context.Context, tenantID, endpoint string) (billing.EndpointPricing, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pricing[key(tenantID, endpoint)]
	if !ok {
		return billing.EndpointPricing{}, shared.ErrNotFound
	}
	return p, nil
}

// UpsertEndpointPrice stores a pricing rule.
func (s *Store) UpsertEndpointPrice(ctx context.Context, p billing.EndpointPricing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pricing[key(p.TenantID(), p.Endpoint())] = p
	return nil
}

// AppendLedgerEntry appends a billable event.
func (s *Store) AppendLedgerEntry(ctx context.Context, e billing.LedgerEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger = append(s.ledger, e)
	return nil
}

// LedgerLen returns the number of ledger entries (test/inspection helper).
func (s *Store) LedgerLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ledger)
}

// MarkProcessed records an event key, returning false on duplicate.
func (s *Store) MarkProcessed(ctx context.Context, tenantID, eventKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tenantID, eventKey)
	if _, ok := s.processed[k]; ok {
		return false, nil
	}
	s.processed[k] = struct{}{}
	return true, nil
}
