package ses

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsses "github.com/aws/aws-sdk-go-v2/service/ses"

	"hyperliquid-builder-code-bot/internal/config"
)

func NewClient(ctx context.Context, cfg config.AWSConfig) (*awsses.Client, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is nil")
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		return nil, err
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
	client, err := NewClient(ctx, awsCfg)
	if err != nil {
		return nil, err
	}
	return NewNotifier(client, OptionsFromConfig(sesCfg))
}
