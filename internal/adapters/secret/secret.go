// Package secret provides a CredentialStore adapter. Bank credentials are
// isolated per tenant AND per bank — keyed by the composite (tenantID, bankID)
// pair — and never live in code. The store is loaded from configuration/
// environment (a vault adapter is a drop-in replacement). Secrets are returned
// only transiently and must never be logged (threat C1/C4; ADR-0007).
package secret

import (
	"context"
	"sync"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// Store is an in-process CredentialStore keyed by the composite (tenantID,
// bankID) pair. The concrete secret values are injected at construction from
// config/env — not hard-coded.
type Store struct {
	mu sync.RWMutex
	// creds is keyed by credKey(tenantID, bankID). A tenant may hold independent
	// credentials at more than one bank; the bankID subdivides — never relaxes —
	// the tenant scope (ADR-0007 T2).
	creds map[string]ports.BankCredential
}

// credKey builds the composite store key for a (tenantID, bankID) pair. The NUL
// separator can appear in neither a tenant id nor a bank slug, so distinct pairs
// can never collide onto the same key (no prefix/normalisation aliasing, ADR-0007
// T2). An empty bankID is normalised to the default BankIDC6 so a legacy
// single-bank credential and an explicit "c6" credential resolve to the same slot.
func credKey(tenantID, bankID string) string {
	return tenantID + "\x00" + defaultBankID(bankID)
}

// defaultBankID resolves an unspecified (empty) bank to the retro-compatible
// default BankIDC6, so pre-multi-bank config and call sites keep working unchanged
// (ADR-0007). A non-empty bankID is returned verbatim — the registry/allowlist
// validation of the slug lives at the request boundary (SIN-66022), not here.
func defaultBankID(bankID string) string {
	if bankID == "" {
		return ports.BankIDC6
	}
	return bankID
}

// NewStore builds a Store from a credential collection (typically loaded from the
// environment/secret manager at startup). Each credential is re-keyed by its own
// (TenantID, BankID) pair, with an empty BankID normalised to the default
// BankIDC6. For backward compatibility a credential whose TenantID is empty is
// keyed (and stamped) under the MAP KEY, preserving the legacy "map key == tenant"
// convenience used by callers that pass a tenant-keyed map of bare credentials.
func NewStore(creds map[string]ports.BankCredential) *Store {
	cp := make(map[string]ports.BankCredential, len(creds))
	for k, v := range creds {
		if v.TenantID == "" {
			v.TenantID = k
		}
		v.BankID = defaultBankID(v.BankID)
		cp[credKey(v.TenantID, v.BankID)] = v
	}
	return &Store{creds: cp}
}

// GetBankCredential returns the credential for the EXACT (tenantID, bankID) pair.
// The lookup is exact-match with NO fallback: a missing pair returns ErrNotFound
// and never resolves to another bank or another tenant (deny-by-default; threat
// T1/T2). An empty bankID resolves to the default BankIDC6 (retro-compat).
func (s *Store) GetBankCredential(_ context.Context, tenantID, bankID string) (ports.BankCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[credKey(tenantID, bankID)]
	if !ok {
		return ports.BankCredential{}, shared.ErrNotFound
	}
	return c, nil
}

// Set stores/replaces a credential for the (tenantID, c.BankID) pair (used by the
// admin plane / config reload). It stamps the tenant id and normalises an empty
// bank to the default BankIDC6.
func (s *Store) Set(tenantID string, c ports.BankCredential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.TenantID = tenantID
	c.BankID = defaultBankID(c.BankID)
	s.creds[credKey(tenantID, c.BankID)] = c
}

// SetBankCredential implements ports.CredentialWriter: it persists a tenant's
// bank credential for the (tenantID, bankID) pair. The secret is held only in the
// store and never returned, logged or echoed (threat C1/C4). Empty inputs are
// rejected as a validation error without including the secret value in the
// message. An empty bankID is stored under the default BankIDC6 (retro-compat).
func (s *Store) SetBankCredential(_ context.Context, tenantID, bankID, clientID, secret string) error {
	if tenantID == "" {
		return shared.NewValidationError("tenant_id", "is required")
	}
	if clientID == "" {
		return shared.NewValidationError("client_id", "is required")
	}
	if secret == "" {
		return shared.NewValidationError("secret", "is required")
	}
	bankID = defaultBankID(bankID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[credKey(tenantID, bankID)] = ports.BankCredential{TenantID: tenantID, BankID: bankID, ClientID: clientID, Secret: secret}
	return nil
}
