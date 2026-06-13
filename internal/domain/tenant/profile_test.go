package tenant_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

var refTime = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

const validCNPJ = "11222333000181"

func TestNormalizeCNPJ(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
		field   string
	}{
		{name: "masked ok", in: "11.222.333/0001-81", want: validCNPJ},
		{name: "digits ok", in: validCNPJ, want: validCNPJ},
		{name: "spaces ok", in: "  11222333000181 ", want: validCNPJ},
		{name: "empty", in: "", wantErr: true, field: "cnpj"},
		{name: "too short", in: "123", wantErr: true, field: "cnpj"},
		{name: "too long", in: "112223330001810", wantErr: true, field: "cnpj"},
		{name: "all same digit", in: "11111111111111", wantErr: true, field: "cnpj"},
		{name: "bad check digit", in: "11222333000180", wantErr: true, field: "cnpj"},
		{name: "non digits stripped to short", in: "abc", wantErr: true, field: "cnpj"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tenant.NormalizeCNPJ(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				if !errors.Is(err, shared.ErrValidation) {
					t.Fatalf("want ErrValidation, got %v", err)
				}
				var ve *shared.ValidationError
				if errors.As(err, &ve) && ve.Field != tt.field {
					t.Fatalf("field = %q, want %q", ve.Field, tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewWithProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		id      string
		p       tenant.Profile
		wantErr string // "" => no error; otherwise expected field
	}{
		{name: "ok full", id: "t1", p: tenant.Profile{Name: "Acme LTDA", DisplayName: "Acme", CNPJ: "11.222.333/0001-81", Email: "ops@acme.com"}},
		{name: "ok display defaults to name", id: "t2", p: tenant.Profile{Name: "Beta SA", CNPJ: validCNPJ, Email: "a@b.co"}},
		{name: "empty id", id: " ", p: tenant.Profile{Name: "X", CNPJ: validCNPJ, Email: "a@b.co"}, wantErr: "id"},
		{name: "empty name", id: "t3", p: tenant.Profile{Name: "  ", CNPJ: validCNPJ, Email: "a@b.co"}, wantErr: "name"},
		{name: "bad cnpj", id: "t4", p: tenant.Profile{Name: "X", CNPJ: "123", Email: "a@b.co"}, wantErr: "cnpj"},
		{name: "bad email", id: "t5", p: tenant.Profile{Name: "X", CNPJ: validCNPJ, Email: "nope"}, wantErr: "email"},
		{name: "empty email", id: "t6", p: tenant.Profile{Name: "X", CNPJ: validCNPJ, Email: ""}, wantErr: "email"},
		{name: "display too long", id: "t7", p: tenant.Profile{Name: "X", DisplayName: longStr(201), CNPJ: validCNPJ, Email: "a@b.co"}, wantErr: "display_name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tenant.NewWithProfile(tt.id, tt.p, refTime)
			if tt.wantErr != "" {
				var ve *shared.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("want validation error, got %v", err)
				}
				if ve.Field != tt.wantErr {
					t.Fatalf("field = %q, want %q", ve.Field, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Active() {
				t.Fatal("new tenant should be active")
			}
			if got.CNPJ() != validCNPJ {
				t.Fatalf("cnpj = %q, want normalised %q", got.CNPJ(), validCNPJ)
			}
			if got.DisplayName() == "" {
				t.Fatal("display name should never be empty")
			}
		})
	}
}

func TestDisplayNameFallback(t *testing.T) {
	t.Parallel()
	tr := tenant.RehydrateProfile("id", "Legal Name", "", "", "", true, refTime)
	if tr.DisplayName() != "Legal Name" {
		t.Fatalf("display name fallback = %q, want legal name", tr.DisplayName())
	}
	tr2 := tenant.RehydrateProfile("id", "Legal", "Nick", validCNPJ, "e@x.co", true, refTime)
	if tr2.DisplayName() != "Nick" {
		t.Fatalf("display name = %q, want Nick", tr2.DisplayName())
	}
	if tr2.Email() != "e@x.co" || tr2.CNPJ() != validCNPJ {
		t.Fatal("profile getters mismatch")
	}
}

func TestActivateDeactivate(t *testing.T) {
	t.Parallel()
	tr, err := tenant.New("id", "Co", refTime)
	if err != nil {
		t.Fatal(err)
	}
	tr.Deactivate()
	if tr.Active() {
		t.Fatal("should be suspended")
	}
	tr.Activate()
	if !tr.Active() {
		t.Fatal("should be active again")
	}
}

func longStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
