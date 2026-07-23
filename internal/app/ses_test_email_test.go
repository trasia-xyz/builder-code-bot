package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"builder-code-bot/internal/config"
	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/logging"
	"builder-code-bot/internal/notification"
)

func TestSendSESTestEmailSendsConfiguredMessageWhenNotificationsDisabled(t *testing.T) {
	cfg := config.Config{
		AWS: config.AWSConfig{Region: "ap-northeast-1"},
		Notification: config.NotificationConfig{
			Enabled: false,
			SES: config.SESConfig{
				From: "sender@example.com",
				To:   []string{"operator@example.com"},
			},
		},
	}
	fake := &recordingNotifier{}
	factoryCalls := 0

	err := sendSESTestEmail(context.Background(), cfg, func(_ context.Context, awsCfg config.AWSConfig, sesCfg config.SESConfig) (notification.Notifier, error) {
		factoryCalls++
		if awsCfg.Region != cfg.AWS.Region || sesCfg.From != cfg.Notification.SES.From {
			t.Fatalf("factory config = %#v, %#v", awsCfg, sesCfg)
		}
		return fake, nil
	})
	if err != nil {
		t.Fatalf("sendSESTestEmail() error = %v", err)
	}
	if factoryCalls != 1 || fake.calls != 1 {
		t.Fatalf("factory calls = %d, notify calls = %d", factoryCalls, fake.calls)
	}
	if fake.message.Status != notification.StatusInfo ||
		fake.message.Subject != sesTestSubject ||
		fake.message.HTMLBody != sesTestBody {
		t.Fatalf("message = %#v", fake.message)
	}
}

func TestSendSESTestEmailReturnsDeliveryError(t *testing.T) {
	boom := errors.New("delivery failed")
	err := sendSESTestEmail(context.Background(), config.Config{}, func(context.Context, config.AWSConfig, config.SESConfig) (notification.Notifier, error) {
		return &recordingNotifier{err: boom}, nil
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "send SES test email") {
		t.Fatalf("sendSESTestEmail() error = %v", err)
	}
}

func TestSendSESTestEmailRejectsNilNotifier(t *testing.T) {
	err := sendSESTestEmail(context.Background(), config.Config{}, func(context.Context, config.AWSConfig, config.SESConfig) (notification.Notifier, error) {
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "notifier is nil") {
		t.Fatalf("sendSESTestEmail() error = %v", err)
	}
}

func TestDispatcherFundingNotifierMapsAlertSeverity(t *testing.T) {
	fake := &recordingNotifier{}
	notifier := dispatcherFundingNotifier{
		dispatcher: notification.NewDispatcher(fake, logging.Logger{}),
	}

	notifier.Alert(
		context.Background(), "warning",
		funding.AlertSeverityWarning, "warning message",
	)
	if fake.message.Status != notification.StatusWarning {
		t.Fatalf("warning status = %q", fake.message.Status)
	}

	notifier.Alert(
		context.Background(), "critical",
		funding.AlertSeverityCritical, "critical message",
	)
	if fake.message.Status != notification.StatusCritical {
		t.Fatalf("critical status = %q", fake.message.Status)
	}
}

type recordingNotifier struct {
	message notification.Message
	err     error
	calls   int
}

func (n *recordingNotifier) Notify(_ context.Context, message notification.Message) error {
	n.calls++
	n.message = message
	return n.err
}
