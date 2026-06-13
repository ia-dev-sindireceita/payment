package tenant

import (
	"strings"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// NormalizeCNPJ accepts a CNPJ with or without mask (Postel's law), strips all
// non-digit characters and validates it: 14 digits with valid check digits. It
// returns the normalised (digits-only) form or a *shared.ValidationError whose
// message names the correction needed (per the wireframe's validation copy).
func NormalizeCNPJ(raw string) (string, error) {
	digits := onlyDigits(raw)
	if digits == "" {
		return "", shared.NewValidationError("cnpj", "CNPJ é obrigatório")
	}
	if len(digits) != 14 {
		return "", shared.NewValidationError("cnpj", "CNPJ deve ter 14 dígitos")
	}
	if allSameDigit(digits) || !validCNPJCheckDigits(digits) {
		return "", shared.NewValidationError("cnpj", "CNPJ inválido — confira os dígitos")
	}
	return digits, nil
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func allSameDigit(d string) bool {
	for i := 1; i < len(d); i++ {
		if d[i] != d[0] {
			return false
		}
	}
	return true
}

// validCNPJCheckDigits verifies the two trailing check digits using the standard
// mod-11 weighting for CNPJ. d must be 14 ASCII digits.
func validCNPJCheckDigits(d string) bool {
	weights1 := []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	weights2 := []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	return d[12] == checkDigit(d[:12], weights1) && d[13] == checkDigit(d[:13], weights2)
}

func checkDigit(d string, weights []int) byte {
	sum := 0
	for i := 0; i < len(weights); i++ {
		sum += int(d[i]-'0') * weights[i]
	}
	rem := sum % 11
	if rem < 2 {
		return '0'
	}
	return byte('0' + (11 - rem))
}

// validateEmail is a deliberately lenient check: a contact e-mail must contain a
// single "@" with non-empty local and domain parts containing a dot. The goal is
// to catch obvious typos, not to fully parse RFC 5322.
func validateEmail(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return shared.NewValidationError("email", "e-mail de contato é obrigatório")
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') {
		return shared.NewValidationError("email", "e-mail inválido")
	}
	domain := email[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return shared.NewValidationError("email", "e-mail inválido")
	}
	return nil
}
