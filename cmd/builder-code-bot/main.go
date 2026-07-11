package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"hyperliquid-builder-code-bot/internal/app"
	"hyperliquid-builder-code-bot/internal/logging"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		logging.Error(context.Background(), "builder code bot stopped with an error",
			logging.String("event", "service_failed"),
			logging.String("error", err.Error()),
		)
		os.Exit(1)
	}
}

func run(args []string) (err error) {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	service, err := app.New(ctx, opts)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, service.Close())
	}()
	if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func parseOptions(args []string) (app.Options, error) {
	var opts app.Options
	opts.ConfigPath = "./config.toml"
	fs := flag.NewFlagSet("builder-code-bot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "configuration file path")
	fs.Var((*presenceFlag)(&opts.RunOnStart), "run-on-start", "run one funding cycle during startup")
	if err := fs.Parse(args); err != nil {
		return app.Options{}, err
	}
	if fs.NArg() != 0 {
		return app.Options{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	return opts, nil
}

type presenceFlag bool

func (f *presenceFlag) Set(string) error {
	*f = true
	return nil
}

func (f *presenceFlag) String() string {
	if f != nil && bool(*f) {
		return "true"
	}
	return "false"
}

func (*presenceFlag) IsBoolFlag() bool { return true }
