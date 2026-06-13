// Package app contains the application services (use-cases / input ports). They
// orchestrate the pure domain and the output ports. This layer MUST NOT import
// database/sql, net/http or vendor SDKs — only domain types and ports.
package app

import "github.com/ia-dev-sindireceita/payment/internal/ports"

// Topics published by the application services.
const (
	TopicPaymentCreated = "payment.created"
	TopicPaymentPaid    = "payment.paid"
)

// Deps bundles the output ports the application services depend on. Each service
// takes only the narrow set it needs; Deps is a convenience for wiring in cmd.
type Deps struct {
	Payments    ports.PaymentRepository
	Tenants     ports.TenantRepository
	Pricing     ports.PricingRepository
	Ledger      ports.LedgerRepository
	Processed   ports.ProcessedEventStore
	Bus         ports.MessageBus
	Bank        ports.BankProvider
	Credentials ports.CredentialStore
	// CredWriter is the admin-plane write path for per-tenant bank credentials.
	// Kept separate from Credentials (the reader) so each service depends only on
	// the capability it needs.
	CredWriter ports.CredentialWriter
	Clock      ports.Clock
	IDs        ports.IDProvider
}
