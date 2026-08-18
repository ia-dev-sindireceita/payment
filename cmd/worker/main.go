// Command worker is the async consumer entrypoint. It subscribes to platform
// events on the MessageBus port and processes them with idempotent handlers,
// shutting down gracefully on signal. The bus adapter (RabbitMQ in production,
// in-memory for local/dev) is chosen here without the domain knowing.
//
// Webhook sweep (SIN-69590 / B2). Beyond the event bus the worker runs a periodic
// background sweep that calls WebhookRegistrationService.TryRegister for every
// tenant. TryRegister is GET-gated (it skips if C6 already holds a callback URL
// under our base origin) and best-effort (it never returns an error), so the sweep
// is safe to call at any cadence and picks up tenants whose in-flow registration
// failed transiently (C6 down, cert not yet uploaded, etc.). Configure with
// PAYMENT_WEBHOOK_SWEEP_INTERVAL (Go duration string, default 5m). Requires
// PAYMENT_BANK_VAULT_KEY for durable tenant listing; without it the sweep is a no-op.
// Requires PAYMENT_C6_BASE_URL + PAYMENT_C6_CLIENT_CERT/KEY for real C6 registration.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"log/slog"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/bank/c6"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/messaging/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/sqlite"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/system"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
	"github.com/ia-dev-sindireceita/payment/internal/platform/config"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
	"github.com/ia-dev-sindireceita/payment/migrations"
)

const defaultSweepInterval = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.FromEnv()

	// Event bus (scaffold: in-memory; swap for RabbitMQ when a broker is configured).
	bus := inmemory.NewBus()
	handler := func(ctx context.Context, m ports.Message) error {
		log.Printf("worker: event type=%s tenant=%s", m.Type, m.TenantID)
		return nil
	}
	for _, topic := range []string{app.TopicPaymentCreated, app.TopicPaymentPaid} {
		if err := bus.Subscribe(ctx, topic, handler); err != nil {
			return err
		}
	}

	// Wire the periodic webhook sweep. All its dependencies are optional: when
	// PAYMENT_BANK_VAULT_KEY is unset the sweep is logged and skipped; when
	// PAYMENT_C6_BASE_URL is unset TryRegister is internally a no-op.
	sweep, closeDB, err := buildWebhookSweep(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeDB()

	sweepInterval := parseSweepInterval()
	log.Printf("worker: started; webhook sweep interval=%s", sweepInterval)

	go runWebhookSweep(ctx, sweep, sweepInterval)

	<-ctx.Done()
	log.Print("worker: shutdown signal received")
	return nil
}

// webhookSweep holds the wired dependencies for the periodic registration sweep.
// When disabled is true the sweep logs a message and returns without doing anything.
type webhookSweep struct {
	disabled bool
	tenants  tenantStore
	svc      *app.WebhookRegistrationService
}

// tenantStore is the narrow port the sweep needs: list every tenant.
type tenantStore interface {
	ListTenants(ctx context.Context) ([]*tenant.Tenant, error)
}

// buildWebhookSweep wires the sweep from the environment. When PAYMENT_BANK_VAULT_KEY
// is unset it returns a disabled sweep (no-op) and a no-op closer. The caller owns
// calling the returned closer.
func buildWebhookSweep(ctx context.Context, cfg config.Config) (webhookSweep, func(), error) {
	noop := func() {}

	if cfg.BankVaultKey == "" {
		log.Print("worker: PAYMENT_BANK_VAULT_KEY unset — webhook sweep disabled (no durable tenant listing)")
		return webhookSweep{disabled: true}, noop, nil
	}

	key, err := hex.DecodeString(cfg.BankVaultKey)
	if err != nil {
		return webhookSweep{}, noop, fmt.Errorf("worker: PAYMENT_BANK_VAULT_KEY is not valid hex")
	}
	cipher, err := secret.NewCipher(key)
	if err != nil {
		return webhookSweep{}, noop, fmt.Errorf("worker: PAYMENT_BANK_VAULT_KEY invalid: %w", err)
	}

	db, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		return webhookSweep{}, noop, err
	}
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		_ = db.Close()
		return webhookSweep{}, noop, err
	}
	closeDB := func() { _ = db.Close() }

	clock := system.Clock{}
	vault := sqlite.NewCredentialVault(db, cipher, clock)
	if err := vault.Seed(ctx, cfg.BankCreds); err != nil {
		closeDB()
		return webhookSweep{}, noop, fmt.Errorf("worker: seed credential vault: %w", err)
	}

	credStore := ports.CredentialStore(secret.NewFallbackStore(vault, secret.NewStore(cfg.BankCreds)))
	refStore := sqlite.NewWebhookRefStore(db, clock)
	minter := app.NewWebhookRefMintService(refStore)
	registrar := buildPixRegistrar(cfg, credStore)
	baseURL := resolveBaseURL()

	svc := app.NewWebhookRegistrationService(credStore, registrar, minter, baseURL, slog.Default())
	return webhookSweep{tenants: sqlite.NewStore(db), svc: svc}, closeDB, nil
}

// buildPixRegistrar returns the real C6 PixWebhookRegistrar when
// PAYMENT_C6_BASE_URL is set, or nil otherwise. A nil registrar causes
// TryRegister to be a no-op (handled inside WebhookRegistrationService.ready).
func buildPixRegistrar(cfg config.Config, creds ports.CredentialStore) ports.PixWebhookRegistrar {
	if cfg.C6.BaseURL == "" {
		log.Print("worker: PAYMENT_C6_BASE_URL not set — webhook registrar in stub mode (TryRegister is a no-op)")
		return nil
	}
	c6cfg := c6.Config{
		BaseURL:  cfg.C6.BaseURL,
		TokenURL: cfg.C6.TokenURL,
		Scope:    cfg.C6.Scope,
		Timeout:  cfg.C6.Timeout,
	}
	if cfg.C6.ClientCertPath != "" && cfg.C6.ClientKeyPath != "" {
		httpc, err := c6.MTLSHTTPClient(cfg.C6.ClientCertPath, cfg.C6.ClientKeyPath, cfg.C6.Timeout)
		if err != nil {
			log.Printf("worker: mTLS client failed — webhook registrar disabled: %v", err)
			return nil
		}
		c6cfg.HTTPClient = httpc
		log.Print("worker: C6 mTLS transport wired for webhook sweep")
	} else {
		log.Print("worker: PAYMENT_C6_CLIENT_CERT/KEY not set — C6 HTTP client without client cert")
		c6cfg.HTTPClient = &stdhttp.Client{Timeout: cfg.C6.Timeout}
	}
	provider, err := c6.New(c6cfg, creds)
	if err != nil {
		log.Printf("worker: C6 provider init failed — webhook registrar disabled: %v", err)
		return nil
	}
	return provider
}

// runWebhookSweep calls sweepOnce on every tick until ctx is cancelled.
func runWebhookSweep(ctx context.Context, s webhookSweep, interval time.Duration) {
	if s.disabled {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepOnce(ctx, s)
		}
	}
}

func sweepOnce(ctx context.Context, s webhookSweep) {
	tenants, err := s.tenants.ListTenants(ctx)
	if err != nil {
		log.Printf("worker: webhook sweep: list tenants failed: %v", err)
		return
	}
	for _, t := range tenants {
		s.svc.TryRegister(ctx, t.ID())
	}
	if len(tenants) > 0 {
		log.Printf("worker: webhook sweep: checked %d tenant(s)", len(tenants))
	}
}

func parseSweepInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("PAYMENT_WEBHOOK_SWEEP_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("worker: PAYMENT_WEBHOOK_SWEEP_INTERVAL=%q invalid, using default %s", v, defaultSweepInterval)
	}
	return defaultSweepInterval
}

func resolveBaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PAYMENT_WEBHOOK_BASE_URL")), "/"); v != "" {
		return v
	}
	return "https://payment.lmhost.com.br"
}
