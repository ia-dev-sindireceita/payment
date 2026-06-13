// Command api is the HTTP entrypoint: it wires the adapters (SQLite persistence,
// bank stub, in-memory bus) behind the application services and serves the
// tenant API, admin plane and bank webhook with graceful shutdown.
//
// Adapter plugability: switching persistence from SQLite to the in-memory store
// (or the bus to RabbitMQ) is a change here only — the domain and use-cases are
// untouched.
package main

import (
	"context"
	"errors"
	"log"
	stdhttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/bank"
	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/messaging/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/sqlite"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/system"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/platform/config"
	"github.com/ia-dev-sindireceita/payment/migrations"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("api: %v", err)
	}
}

func run() error {
	cfg := config.FromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(ctx, db, migrations.FS); err != nil {
		return err
	}

	store := sqlite.NewStore(db)
	creds := secret.NewStore(cfg.BankCreds)
	deps := app.Deps{
		Payments:    store,
		Tenants:     store,
		Pricing:     store,
		Ledger:      store,
		Processed:   store,
		Bus:         inmemory.NewBus(),
		Bank:        bank.NewStubProvider(creds),
		Credentials: creds,
		Clock:       system.Clock{},
		IDs:         system.IDProvider{},
	}

	auth := httpadapter.NewStaticTokenAuth(cfg.TenantTokens, cfg.AdminTokens, cfg.WebhookSecret)
	srv := httpadapter.NewServer(httpadapter.Config{
		Charges:     app.NewChargeService(deps),
		Admin:       app.NewAdminService(deps),
		Webhooks:    app.NewWebhookService(deps),
		TenantAuth:  auth,
		AdminAuth:   auth,
		WebhookAuth: auth,
	})

	httpServer := &stdhttp.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api: listening on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Print("api: shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
