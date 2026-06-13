// Package config parses process configuration from the environment. Secrets
// (tokens, webhook secret, bank credentials) come from the environment / secret
// manager — never from code or URLs (threat C1). A real deployment swaps this
// for a vault-backed loader; the shape stays the same.
package config

import (
	"os"
	"strings"

	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// Config is the resolved process configuration.
type Config struct {
	HTTPAddr       string
	DBPath         string
	TenantTokens   map[string]string // token -> tenantID
	AdminTokens    []string          // full-access admin tokens (RoleAdmin)
	OperatorTokens []string          // read-only admin tokens (RoleOperator)
	WebhookSecret  string
	BankCreds      map[string]ports.BankCredential // tenantID -> credential
	RabbitURL      string
}

// FromEnv builds a Config from environment variables, applying safe defaults.
func FromEnv() Config {
	return Config{
		HTTPAddr:       getenv("PAYMENT_HTTP_ADDR", ":8080"),
		DBPath:         getenv("PAYMENT_DB_PATH", "payment.db"),
		TenantTokens:   parseKV(os.Getenv("PAYMENT_TENANT_TOKENS")),
		AdminTokens:    splitNonEmpty(os.Getenv("PAYMENT_ADMIN_TOKENS")),
		OperatorTokens: splitNonEmpty(os.Getenv("PAYMENT_OPERATOR_TOKENS")),
		WebhookSecret:  os.Getenv("PAYMENT_WEBHOOK_SECRET"),
		BankCreds:      parseBankCreds(os.Getenv("PAYMENT_BANK_CREDS")),
		RabbitURL:      os.Getenv("PAYMENT_RABBIT_URL"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseKV parses "k1:v1,k2:v2" into a map. Malformed pairs are skipped.
func parseKV(s string) map[string]string {
	m := make(map[string]string)
	for _, pair := range splitNonEmpty(s) {
		k, v, ok := strings.Cut(pair, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if ok && k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

// parseBankCreds parses "tenant:clientID:secret,..." into per-tenant credentials.
func parseBankCreds(s string) map[string]ports.BankCredential {
	m := make(map[string]ports.BankCredential)
	for _, item := range splitNonEmpty(s) {
		parts := strings.SplitN(item, ":", 3)
		if len(parts) != 3 {
			continue
		}
		tenant := strings.TrimSpace(parts[0])
		if tenant == "" {
			continue
		}
		m[tenant] = ports.BankCredential{
			TenantID: tenant,
			ClientID: strings.TrimSpace(parts[1]),
			Secret:   strings.TrimSpace(parts[2]),
		}
	}
	return m
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
