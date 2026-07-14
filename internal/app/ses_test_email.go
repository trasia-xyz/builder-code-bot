package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"builder-code-bot/internal/config"
	"builder-code-bot/internal/notification"
	"builder-code-bot/internal/notification/mail/ses"
)

const (
	sesTestSubject = "Amazon SES connectivity test"
	sesTestBody    = "This is a test email sent by builder-code-bot to verify Amazon SES connectivity."
)

type sesNotifierFactory func(context.Context, config.AWSConfig, config.SESConfig) (notification.Notifier, error)

// SendSESTestEmail sends one real email using the configured SES sender and
// recipients. It intentionally ignores notification.enabled so operators can
// verify SES before enabling runtime notifications.
func SendSESTestEmail(ctx context.Context, configPath string) error {
	if ctx == nil {
		return fmt.Errorf("SES test context is nil")
	}
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger := configureLogger(cfg)
	if err := sendSESTestEmail(ctx, cfg, func(ctx context.Context, awsCfg config.AWSConfig, sesCfg config.SESConfig) (notification.Notifier, error) {
		return ses.New(ctx, awsCfg, sesCfg)
	}); err != nil {
		return err
	}
	logger.Info(ctx, "SES test email sent",
		slog.String("event", "ses_test_email_sent"),
		slog.String("aws_region", cfg.AWS.Region),
		slog.Int("recipient_count", len(cfg.Notification.SES.To)),
	)
	return nil
}

func sendSESTestEmail(ctx context.Context, cfg config.Config, factory sesNotifierFactory) error {
	if ctx == nil {
		return fmt.Errorf("SES test context is nil")
	}
	if factory == nil {
		return fmt.Errorf("SES notifier factory is nil")
	}
	notifier, err := factory(ctx, cfg.AWS, cfg.Notification.SES)
	if err != nil {
		return fmt.Errorf("initialize SES test notifier: %w", err)
	}
	if notifier == nil {
		return fmt.Errorf("initialize SES test notifier: notifier is nil")
	}
	if err := notifier.Notify(ctx, notification.Message{Subject: sesTestSubject, Body: sesTestBody}); err != nil {
		return fmt.Errorf("send SES test email: %w", err)
	}
	return nil
}
