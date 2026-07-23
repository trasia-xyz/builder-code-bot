package ses

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsses "github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"

	"builder-code-bot/internal/notification"
	notificationmail "builder-code-bot/internal/notification/mail"
)

const charset = "UTF-8"

var (
	ErrNilClient   = errors.New("notification mail ses: client is nil")
	ErrNilResponse = errors.New("notification mail ses: send email response is nil")
)

type api interface {
	SendEmail(context.Context, *awsses.SendEmailInput, ...func(*awsses.Options)) (*awsses.SendEmailOutput, error)
}

type Options struct {
	Source        string
	To            []string
	SubjectPrefix string
}

type Notifier struct {
	client api
	opts   Options
}

var _ notification.Notifier = (*Notifier)(nil)

func NewNotifier(client api, opts Options) (*Notifier, error) {
	if client == nil {
		return nil, ErrNilClient
	}
	var err error
	opts, err = validateAndNormalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	return &Notifier{client: client, opts: opts}, nil
}

func validateAndNormalizeOptions(opts Options) (Options, error) {
	opts = normalizeOptions(opts)
	if err := validateOptions(opts); err != nil {
		return Options{}, err
	}
	return opts, nil
}

func (n *Notifier) Notify(ctx context.Context, msg notification.Message) error {
	if ctx == nil {
		return fmt.Errorf("context is nil")
	}
	if n == nil || n.client == nil {
		return ErrNilClient
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	input := &awsses.SendEmailInput{
		Source:      aws.String(n.opts.Source),
		Destination: &types.Destination{ToAddresses: cloneStrings(n.opts.To)},
		Message: &types.Message{
			Subject: content(prefixedSubject(msg.Status.Indicator(), n.opts.SubjectPrefix, msg.Subject)),
			Body:    &types.Body{Html: content(messageHTML(msg))},
		},
	}
	out, err := n.client.SendEmail(ctx, input)
	if err != nil {
		return fmt.Errorf("send ses email: %w", err)
	}
	if out == nil {
		return ErrNilResponse
	}
	return nil
}

func normalizeOptions(opts Options) Options {
	opts.Source = notificationmail.NormalizeAddress(opts.Source)
	opts.To = notificationmail.NormalizeAddressList(opts.To)
	opts.SubjectPrefix = strings.TrimSpace(opts.SubjectPrefix)
	return opts
}

func validateOptions(opts Options) error {
	var errs []error
	if opts.Source == "" {
		errs = append(errs, fmt.Errorf("ses source is required"))
	} else {
		errs = append(errs, notificationmail.ValidateAddress("ses source", opts.Source))
	}
	if len(opts.To) == 0 {
		errs = append(errs, fmt.Errorf("ses to recipients are required"))
	}
	for i, value := range opts.To {
		errs = append(errs, notificationmail.ValidateAddress(fmt.Sprintf("ses to[%d]", i), value))
	}
	return errors.Join(errs...)
}

func content(data string) *types.Content {
	return &types.Content{Data: aws.String(data), Charset: aws.String(charset)}
}

func prefixedSubject(indicator, prefix, subject string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{indicator, prefix, subject} {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

func messageHTML(msg notification.Message) string {
	if body := strings.TrimSpace(msg.HTMLBody); body != "" {
		return body
	}
	body := html.EscapeString(strings.TrimSpace(msg.Body))
	body = strings.ReplaceAll(body, "\n", "<br>")
	return `<!doctype html><html><body style="margin:0;background:#f5f7fa;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;">` +
		`<div style="max-width:640px;margin:0 auto;padding:24px 16px;">` +
		`<div style="background:#ffffff;border:1px solid #e5e9f0;border-radius:12px;padding:24px;font-size:15px;line-height:1.65;">` +
		body + `</div></div></body></html>`
}

func cloneStrings(values []string) []string { return append([]string(nil), values...) }
