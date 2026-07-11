package ses

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsses "github.com/aws/aws-sdk-go-v2/service/ses"

	"hyperliquid-builder-code-bot/internal/config"
	"hyperliquid-builder-code-bot/internal/notification"
)

func TestNotifierSendBuildsUTF8PlainTextEmail(t *testing.T) {
	t.Parallel()
	fake := &fakeSES{messageID: "message-1"}
	n, err := NewNotifier(fake, Options{
		Source: " Trasia <alerts@example.com> ", To: []string{" ops@example.com ", "dev@example.com"},
		ReplyTo: []string{" support@example.com "}, SubjectPrefix: " [prod] ",
	})
	if err != nil {
		t.Fatalf("NewNotifier() error = %v", err)
	}
	id, err := n.Send(context.Background(), notification.Message{Subject: "mysql down", Body: "attempts: 9"})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if id != "message-1" {
		t.Fatalf("message ID = %q", id)
	}
	got := fake.input
	if aws.ToString(got.Source) != "Trasia <alerts@example.com>" {
		t.Fatalf("source = %q", aws.ToString(got.Source))
	}
	assertStringSlice(t, got.Destination.ToAddresses, []string{"ops@example.com", "dev@example.com"})
	assertStringSlice(t, got.ReplyToAddresses, []string{"support@example.com"})
	if aws.ToString(got.Message.Subject.Data) != "[prod] mysql down" {
		t.Fatalf("subject = %q", aws.ToString(got.Message.Subject.Data))
	}
	if aws.ToString(got.Message.Subject.Charset) != charset {
		t.Fatalf("subject charset = %q", aws.ToString(got.Message.Subject.Charset))
	}
	if got.Message.Body.Html != nil {
		t.Fatal("HTML body is set")
	}
	if aws.ToString(got.Message.Body.Text.Data) != "attempts: 9" {
		t.Fatalf("body = %q", aws.ToString(got.Message.Body.Text.Data))
	}
	if aws.ToString(got.Message.Body.Text.Charset) != charset {
		t.Fatalf("body charset = %q", aws.ToString(got.Message.Body.Text.Charset))
	}
}

func TestOptionsFromConfigCopiesSESSettings(t *testing.T) {
	t.Parallel()
	cfg := config.SESConfig{From: "sender@example.com", To: []string{"one@example.com"}, ReplyTo: []string{"reply@example.com"}, SubjectPrefix: "[bot]"}
	opts := OptionsFromConfig(cfg)
	cfg.To[0] = "changed@example.com"
	if opts.Source != "sender@example.com" || opts.To[0] != "one@example.com" || opts.ReplyTo[0] != "reply@example.com" || opts.SubjectPrefix != "[bot]" {
		t.Fatalf("OptionsFromConfig() = %+v", opts)
	}
}

func TestNewNotifierRejectsEmptyRecipients(t *testing.T) {
	t.Parallel()
	_, err := NewNotifier(&fakeSES{}, Options{Source: "sender@example.com", To: []string{" ", ""}})
	if err == nil || !strings.Contains(err.Error(), "to recipients") {
		t.Fatalf("NewNotifier() error = %v", err)
	}
}

func TestNotifierReturnsWrappedClientError(t *testing.T) {
	t.Parallel()
	boom := errors.New("ses unavailable")
	n, err := NewNotifier(&fakeSES{err: boom}, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = n.Notify(context.Background(), notification.Message{Subject: "subject", Body: "body"})
	if !errors.Is(err, boom) {
		t.Fatalf("Notify() error = %v", err)
	}
}

func TestNotifierRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		client api
		opts   Options
		ctx    context.Context
		msg    notification.Message
		want   string
	}{
		{name: "nil client", opts: validOptions(), ctx: context.Background(), msg: notification.Message{Subject: "s", Body: "b"}, want: ErrNilClient.Error()},
		{name: "nil context", client: &fakeSES{}, opts: validOptions(), msg: notification.Message{Subject: "s", Body: "b"}, want: "context is nil"},
		{name: "empty subject", client: &fakeSES{}, opts: validOptions(), ctx: context.Background(), msg: notification.Message{Body: "b"}, want: "subject"},
		{name: "bad source", client: &fakeSES{}, opts: Options{Source: "bad", To: []string{"ops@example.com"}}, ctx: context.Background(), msg: notification.Message{Subject: "s", Body: "b"}, want: "source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewNotifier(tt.client, tt.opts)
			if err == nil {
				_, err = n.Send(tt.ctx, tt.msg)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNotifierRejectsNilResponse(t *testing.T) {
	t.Parallel()
	n, err := NewNotifier(&fakeSES{nilOutput: true}, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.Send(context.Background(), notification.Message{Subject: "s", Body: "b"})
	if !errors.Is(err, ErrNilResponse) {
		t.Fatalf("Send() error = %v", err)
	}
}

func validOptions() Options {
	return Options{Source: "alerts@example.com", To: []string{"ops@example.com"}}
}

func assertStringSlice(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

type fakeSES struct {
	input     *awsses.SendEmailInput
	messageID string
	err       error
	nilOutput bool
}

func (f *fakeSES) SendEmail(_ context.Context, input *awsses.SendEmailInput, _ ...func(*awsses.Options)) (*awsses.SendEmailOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.input = input
	if f.nilOutput {
		return nil, nil
	}
	return &awsses.SendEmailOutput{MessageId: aws.String(f.messageID)}, nil
}
