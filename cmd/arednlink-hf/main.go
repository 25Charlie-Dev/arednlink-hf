package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"arednlink-hf/internal/app"
	"arednlink-hf/internal/protocol"
)

var version = "development"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		slog.Error("daemon stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("arednlink-hf", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to strict JSON configuration")
	showVersion := flags.Bool("version", false, "print build version")
	encode := flags.String("encode", "", "encode AL1/AL2 as AL2, preserving AL3/AL4 service flags")
	decode := flags.String("decode", "", "decode AL1/AL2/AL3/AL4 and print canonical text")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	visited := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	codecSelected := visited["encode"] || visited["decode"]
	if codecSelected {
		if visited["encode"] && visited["decode"] || visited["version"] || visited["config"] {
			return fmt.Errorf("codec, version, and config flags are mutually exclusive")
		}
		return runCodec(visited["encode"], *encode, *decode, stdout)
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}
	cfg := app.Defaults()
	if *path != "" {
		var err error
		cfg, err = app.LoadConfig(*path)
		if err != nil {
			return err
		}
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("configure log level: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, cfg, logger, *path)
}

func runCodec(encoding bool, encodeInput, decodeInput string, stdout io.Writer) error {
	input := decodeInput
	operation := "decode"
	if encoding {
		input = encodeInput
		operation = "encode"
	}
	if input == "" {
		return fmt.Errorf("%s requires a nonempty advertisement", operation)
	}
	ad, err := protocol.Parse(input)
	if err != nil {
		return err
	}
	if encoding {
		output, err := protocol.EncodeCompact(ad)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, output)
		return err
	}
	canonical, err := ad.Canonical()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, canonical)
	return err
}
