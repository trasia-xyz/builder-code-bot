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

	"builder-code-bot/internal/app"
	"builder-code-bot/internal/logging"
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
	return runWithOutput(args, os.Stdout)
}

func runWithOutput(args []string, stdout io.Writer) (err error) {
	opts, err := parseOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		writeUsage(stdout)
		return nil
	}
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.TestSES {
		return app.SendSESTestEmail(ctx, opts.ConfigPath)
	}
	if opts.ConsolidateOnly {
		return app.ConsolidateFunds(ctx, opts.ConfigPath)
	}

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
	fs := newOptionFlagSet(&opts, io.Discard)
	if err := fs.Parse(args); err != nil {
		return app.Options{}, err
	}
	if fs.NArg() != 0 {
		return app.Options{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if opts.TestSES && opts.RunOnStart {
		return app.Options{}, fmt.Errorf("-test-ses and -run-on-start cannot be used together")
	}
	if opts.ConsolidateOnly && (opts.TestSES || opts.RunOnStart) {
		return app.Options{}, fmt.Errorf("-consolidate-only cannot be used with -test-ses or -run-on-start")
	}
	return opts, nil
}

func newOptionFlagSet(opts *app.Options, output io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("builder-code-bot", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintf(output, "Usage: %s [flags]\n\nAvailable flags:\n", fs.Name())
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "configuration file path")
	fs.BoolVar(&opts.RunOnStart, "run-on-start", false, "run the database payout phase during startup")
	fs.BoolVar(&opts.TestSES, "test-ses", false, "send one SES test email and exit")
	fs.BoolVar(&opts.ConsolidateOnly, "consolidate-only", false, "claim and sweep builder funds, then exit")
	return fs
}

func writeUsage(output io.Writer) {
	opts := app.Options{ConfigPath: "./config.toml"}
	newOptionFlagSet(&opts, output).Usage()
}
