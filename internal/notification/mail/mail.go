package mail

import (
	"fmt"
	netmail "net/mail"
	"strings"
)

func NormalizeAddress(value string) string { return strings.TrimSpace(value) }

func NormalizeAddressList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = NormalizeAddress(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func ValidateAddress(field, value string) error {
	if _, err := netmail.ParseAddress(value); err != nil {
		return fmt.Errorf("%s must be a valid email address: %w", field, err)
	}
	return nil
}
