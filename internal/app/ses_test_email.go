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
	sesTestBody    = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;background:#f3f5f8;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;">
  <div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent;">Amazon SES HTML delivery is working.</div>
  <div style="max-width:640px;margin:0 auto;padding:24px 16px;">
    <div style="background:#eff6ff;border:1px solid #bfdbfe;border-radius:14px;padding:28px;">
      <div style="font-size:12px;font-weight:700;letter-spacing:.08em;color:#2563eb;">BUILDER CODE BOT</div>
      <div style="margin-top:8px;font-size:24px;font-weight:750;color:#1d4ed8;">SES HTML delivery is working</div>
      <div style="margin-top:10px;font-size:14px;line-height:1.6;color:#475569;">This email verifies AWS credentials, Amazon SES delivery, UTF-8 subjects, HTML rendering, and inbox preview text.</div>
    </div>
  </div>
</body>
</html>`
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
	if err := notifier.Notify(ctx, notification.Message{
		Status:   notification.StatusInfo,
		Subject:  sesTestSubject,
		HTMLBody: sesTestBody,
	}); err != nil {
		return fmt.Errorf("send SES test email: %w", err)
	}
	return nil
}
