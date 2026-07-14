package ses

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsses "github.com/aws/aws-sdk-go-v2/service/ses"

	"builder-code-bot/internal/config"
)

func NewClient(ctx context.Context, cfg config.AWSConfig) (*awsses.Client, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is nil")
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("AWS region is required for SES")
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.Profile != "" {
		loadOptions = append(loadOptions, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return awsses.NewFromConfig(loaded), nil
}

func New(ctx context.Context, awsCfg config.AWSConfig, sesCfg config.SESConfig) (*Notifier, error) {
	opts, err := validateAndNormalizeOptions(Options{
		Source: sesCfg.From, To: cloneStrings(sesCfg.To), ReplyTo: cloneStrings(sesCfg.ReplyTo),
		SubjectPrefix: sesCfg.SubjectPrefix,
	})
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ctx, awsCfg)
	if err != nil {
		return nil, err
	}
	return &Notifier{client: client, opts: opts}, nil
}
