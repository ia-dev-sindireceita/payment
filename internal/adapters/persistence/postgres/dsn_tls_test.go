package postgres

import (
	"strings"
	"testing"
)

// TestAssertTransportSecurity is the guard that fails the boot when a Postgres DSN
// would carry the Vault-issued password and PII across the private VLAN in
// cleartext (SIN-70320, A02). Remote hosts must require TLS; loopback / local
// sockets are exempt because the bytes never leave the box — which is also how the
// adapter test harness and dev/CI connect (sslmode=disable to 127.0.0.1).
func TestAssertTransportSecurity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		// Remote target: TLS is mandatory.
		{"remote require", "postgres://u:p@172.18.2.248:5432/payment?sslmode=require", false},
		{"remote verify-ca", "postgres://u:p@172.18.2.248:5432/payment?sslmode=verify-ca", false},
		{"remote verify-full", "postgres://u:p@172.18.2.248:5432/payment?sslmode=verify-full", false},
		{"remote disable rejected", "postgres://u:p@172.18.2.248:5432/payment?sslmode=disable", true},
		{"remote allow rejected", "postgres://u:p@172.18.2.248:5432/payment?sslmode=allow", true},
		{"remote prefer rejected", "postgres://u:p@172.18.2.248:5432/payment?sslmode=prefer", true},
		{"remote default (no sslmode) rejected", "postgres://u:p@172.18.2.248:5432/payment", true},
		{"remote hostname disable rejected", "postgres://u:p@data.lmhost.com.br:5432/payment?sslmode=disable", true},
		// keyword/value DSN form.
		{"kv remote require", "host=172.18.2.248 port=5432 user=u password=p dbname=payment sslmode=require", false},
		{"kv remote disable rejected", "host=172.18.2.248 port=5432 user=u password=p dbname=payment sslmode=disable", true},
		// Local targets: exempt (loopback / localhost / unix socket / unset).
		{"loopback v4 disable ok", "postgres://u:p@127.0.0.1:5432/payment?sslmode=disable", false},
		{"loopback v6 disable ok", "postgres://u:p@[::1]:5432/payment?sslmode=disable", false},
		{"localhost disable ok", "postgres://u:p@localhost:5432/payment?sslmode=disable", false},
		{"kv localhost disable ok", "host=localhost port=5432 user=u dbname=payment sslmode=disable", false},
		{"kv unix socket ok", "host=/var/run/postgresql port=5432 user=u dbname=payment sslmode=disable", false},
		// Multi-host DSNs (primary + fallback): every target the driver would dial is
		// mediated, not just the primary (SIN-70355). A local primary must not smuggle
		// a remote cleartext fallback past the guard.
		{"multihost local primary remote fallback rejected", "host=localhost,172.18.2.248 port=5432 user=u dbname=payment sslmode=disable", true},
		{"multihost all-local ok", "host=localhost,127.0.0.1 port=5432 user=u dbname=payment sslmode=disable", false},
		{"multihost remote require ok", "host=172.18.2.248,172.18.2.249 port=5432 user=u dbname=payment sslmode=require", false},
		// Malformed DSN fails closed (an error, never a silent pass).
		{"garbage dsn", "://not a dsn at all ???", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := assertTransportSecurity(tc.dsn)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.dsn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.dsn, err)
			}
		})
	}
}

// TestAssertTransportSecurityErrorHidesSecret makes sure the failure names the host
// but never leaks the DSN password into the boot log / error chain.
func TestAssertTransportSecurityErrorHidesSecret(t *testing.T) {
	t.Parallel()
	const secret = "sup3r-s3cret-pw"
	err := assertTransportSecurity("postgres://payment:" + secret + "@172.18.2.248:5432/payment?sslmode=disable")
	if err == nil {
		t.Fatal("expected rejection of remote plaintext DSN")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must not echo the DSN password: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "172.18.2.248") {
		t.Fatalf("error should name the offending host: %q", err.Error())
	}
}

// TestOpenRejectsInsecureRemoteDSN proves the guard runs before any connection is
// attempted: Open returns the boot-refusal error without needing a live database.
func TestOpenRejectsInsecureRemoteDSN(t *testing.T) {
	t.Parallel()
	db, err := Open("postgres://u:p@172.18.2.248:5432/payment?sslmode=disable")
	if err == nil {
		if db != nil {
			_ = db.Close()
		}
		t.Fatal("Open should refuse an insecure remote DSN")
	}
	if db != nil {
		t.Fatal("Open must not return a handle when it refuses the DSN")
	}
}
