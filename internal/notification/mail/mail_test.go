package mail

import (
	"strings"
	"testing"
)

func TestNormalizeAddressListTrimsAndDropsEmpty(t *testing.T) {
	t.Parallel()
	got := NormalizeAddressList([]string{" ops@example.com ", "", " \t ", "Dev <dev@example.com>"})
	want := []string{"ops@example.com", "Dev <dev@example.com>"}
	assertStrings(t, got, want)
}

func TestValidateAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, value, wantErr string
	}{
		{name: "mailbox", value: "ops@example.com"},
		{name: "display name", value: "Trasia Ops <ops@example.com>"},
		{name: "invalid", value: "bad", wantErr: "notification.mail.to[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAddress("notification.mail.to[0]", tt.value)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("ValidateAddress() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("ValidateAddress() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("value[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
