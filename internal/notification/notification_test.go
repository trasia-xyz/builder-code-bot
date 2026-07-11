package notification

import (
	"context"
	"strings"
	"testing"
)

func TestMessageValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{name: "valid", msg: Message{Subject: "subject", Body: "body"}},
		{name: "subject", msg: Message{Body: "body"}, want: "subject"},
		{name: "body", msg: Message{Subject: "subject"}, want: "body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.msg.Validate()
			if tt.want == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNoopAlwaysSucceeds(t *testing.T) {
	t.Parallel()
	if err := (Noop{}).Notify(context.Background(), Message{}); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
}
