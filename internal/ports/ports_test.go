package ports_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

const redactSecret = "do-not-print-this-secret"

func newCred() ports.BankCredential {
	return ports.BankCredential{TenantID: "ten-1", ClientID: "cid-9", Secret: redactSecret}
}

// TestBankCredentialStringRedactsSecret asserts no fmt verb that uses Stringer
// (%v/%s/%+v) prints the secret.
func TestBankCredentialStringRedactsSecret(t *testing.T) {
	t.Parallel()
	c := newCred()
	for _, verb := range []string{"%v", "%s", "%+v"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, redactSecret) {
			t.Fatalf("%s leaked the secret: %q", verb, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Fatalf("%s missing redaction marker: %q", verb, out)
		}
		if !strings.Contains(out, "cid-9") || !strings.Contains(out, "ten-1") {
			t.Fatalf("%s dropped non-secret fields: %q", verb, out)
		}
	}
}

// TestBankCredentialLogValueRedactsSecret asserts structured logging never emits
// the secret even when the credential is logged as an attribute value.
func TestBankCredentialLogValueRedactsSecret(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("credential stored", "credential", newCred())

	out := buf.String()
	if strings.Contains(out, redactSecret) {
		t.Fatalf("slog leaked the secret: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("slog missing redaction marker: %q", out)
	}
	if !strings.Contains(out, "cid-9") || !strings.Contains(out, "ten-1") {
		t.Fatalf("slog dropped non-secret fields: %q", out)
	}
}
