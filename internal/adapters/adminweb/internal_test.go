package adminweb

import "testing"

func TestNewTokenIsRandomAndNonEmpty(t *testing.T) {
	t.Parallel()
	a, b := newToken(), newToken()
	if a == "" || b == "" {
		t.Fatal("token must be non-empty")
	}
	if a == b {
		t.Fatal("tokens must differ")
	}
	if len(a) < 40 { // 32 random bytes -> 43 base64url chars
		t.Fatalf("token too short: %d", len(a))
	}
}

func TestMaskCNPJ(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":               "—",
		"11222333000181": "11.222.333/0001-81",
		"123":            "123", // defensive passthrough for unexpected length
	}
	for in, want := range cases {
		if got := maskCNPJ(in); got != want {
			t.Fatalf("maskCNPJ(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapValidationFieldDefault(t *testing.T) {
	t.Parallel()
	if got := mapValidationField("id"); got != "form" {
		t.Fatalf("unmapped field should map to form, got %q", got)
	}
	for _, f := range []string{"name", "cnpj", "email", "display_name"} {
		if got := mapValidationField(f); got != f {
			t.Fatalf("mapValidationField(%q) = %q", f, got)
		}
	}
}
