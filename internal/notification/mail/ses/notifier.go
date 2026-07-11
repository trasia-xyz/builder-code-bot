package ses

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsses "github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"

	"hyperliquid-builder-code-bot/internal/config"
	"hyperliquid-builder-code-bot/internal/notification"
	notificationmail "hyperliquid-builder-code-bot/internal/notification/mail"
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
	ReplyTo       []string
	SubjectPrefix string
}

type Notifier struct {
	client api
	opts   Options
}

var _ notification.Notifier = (*Notifier)(nil)

func OptionsFromConfig(cfg config.SESConfig) Options {
	return Options{
		Source: cfg.From, To: cloneStrings(cfg.To), ReplyTo: cloneStrings(cfg.ReplyTo),
		SubjectPrefix: cfg.SubjectPrefix,
	}
}

func NewNotifier(client api, opts Options) (*Notifier, error) {
	if client == nil {
		return nil, ErrNilClient
	}
	opts = normalizeOptions(opts)
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	return &Notifier{client: client, opts: opts}, nil
}

func (n *Notifier) Notify(ctx context.Context, msg notification.Message) error {
	_, err := n.Send(ctx, msg)
	return err
}

func (n *Notifier) Send(ctx context.Context, msg notification.Message) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("context is nil")
	}
	if n == nil || n.client == nil {
		return "", ErrNilClient
	}
	if err := msg.Validate(); err != nil {
		return "", err
	}
	input := &awsses.SendEmailInput{
		Source:      aws.String(n.opts.Source),
		Destination: &types.Destination{ToAddresses: cloneStrings(n.opts.To)},
		Message: &types.Message{
			Subject: content(prefixedSubject(n.opts.SubjectPrefix, msg.Subject)),
			Body:    &types.Body{Text: content(msg.Body)},
		},
	}
	if len(n.opts.ReplyTo) > 0 {
		input.ReplyToAddresses = cloneStrings(n.opts.ReplyTo)
	}
	out, err := n.client.SendEmail(ctx, input)
	if err != nil {
		return "", fmt.Errorf("send ses email: %w", err)
	}
	if out == nil {
		return "", ErrNilResponse
	}
	return aws.ToString(out.MessageId), nil
}

func normalizeOptions(opts Options) Options {
	opts.Source = notificationmail.NormalizeAddress(opts.Source)
	opts.To = notificationmail.NormalizeAddressList(opts.To)
	opts.ReplyTo = notificationmail.NormalizeAddressList(opts.ReplyTo)
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
	for i, value := range opts.ReplyTo {
		errs = append(errs, notificationmail.ValidateAddress(fmt.Sprintf("ses reply_to[%d]", i), value))
	}
	return errors.Join(errs...)
}

func content(data string) *types.Content {
	return &types.Content{Data: aws.String(data), Charset: aws.String(charset)}
}

func prefixedSubject(prefix, subject string) string {
	prefix, subject = strings.TrimSpace(prefix), strings.TrimSpace(subject)
	if prefix == "" {
		return subject
	}
	return prefix + " " + subject
}

func cloneStrings(values []string) []string { return append([]string(nil), values...) }
