package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer

type closeFunc func() error

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

var sensitiveKeys = []string{
	"password",
	"key",
	"apikey",
	"secret",
	"pin",
	"creditcardno",
	"user",
}

func main() {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)

	cancel()
	os.Exit(status)
}

func run(
	ctx context.Context,
	cancel context.CancelFunc,
	httpPort int,
	dataDir string,
) int {
	tracerProvider, err := initTracing(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tracing: %v\n", err)
		return 1
	}

	defer func() {
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "failed to shutdown tracing: %v\n", err)
		}
	}()

	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))

	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}

	env := os.Getenv("ENV")

	hostname, err := os.Hostname()
	if err != nil {
		logger.Error(
			"failed to get hostname",
			"error", err,
		)
		return 1
	}

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close logger: %v\n", err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(
			"failed to create store",
			"error", err,
		)
		return 1
	}

	s := newServer(*st, httpPort, cancel, logger)

	var serverErr error

	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer shutdownCancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server:", "error", err)
		return 1
	}

	if serverErr != nil {
		logger.Error("server error:", "error", serverErr)
		return 1
	}

	return 0
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {

	a = redactAttr(a)

	if a.Key != "error" {
		return a
	}

	err, ok := a.Value.Any().(error)
	if !ok {
		return a
	}

	var multiErr multiError

	if errors.As(err, &multiErr) {
		var errAttrs []slog.Attr

		for i, err := range multiErr.Unwrap() {
			errAttrs = append(
				errAttrs,
				slog.GroupAttrs(
					fmt.Sprintf("error_%d", i+1),
					errorAttrs(err)...,
				),
			)
		}

		return slog.GroupAttrs(
			"errors",
			errAttrs...,
		)
	}

	return slog.GroupAttrs(
		"error",
		errorAttrs(err)...,
	)
}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	isTTY := isatty.IsTerminal(os.Stderr.Fd()) ||
		isatty.IsCygwinTerminal(os.Stderr.Fd())

	stderrHandler := tint.NewTextHandler(
		os.Stderr,
		&tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
			NoColor:     !isTTY,
		},
	)

	if logFile == "" {
		logger := slog.New(stderrHandler)

		close := func() error {
			return nil
		}

		return logger, close, nil
	}

	fileLogger := &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    1,
		MaxAge:     28,
		MaxBackups: 10,
		LocalTime:  false,
		Compress:   true,
	}

	fileHandler := slog.NewJSONHandler(
		fileLogger,
		&slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		},
	)

	multiHandler := slog.NewMultiHandler(
		stderrHandler,
		fileHandler,
	)

	logger := slog.New(multiHandler)

	close := func() error {
		return fileLogger.Close()
	}

	return logger, close, nil
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("message", err.Error()),
	}

	for _, attr := range linkoerr.Attrs(err) {
		attrs = append(attrs, redactAttr(attr))
	}
	var stackErr stackTracer

	if errors.As(err, &stackErr) {
		attrs = append(
			attrs,
			slog.String(
				"stack_trace",
				fmt.Sprintf("%+v", stackErr.StackTrace()),
			),
		)
	}

	return attrs
}

func redactAttr(a slog.Attr) slog.Attr {
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}

	if a.Value.Kind() != slog.KindString {
		return a
	}

	value := a.Value.String()

	parsedURL, err := url.Parse(value)
	if err != nil || parsedURL.User == nil {
		return a
	}

	if _, hasPassword := parsedURL.User.Password(); !hasPassword {
		return a
	}

	username := parsedURL.User.Username()

	parsedURL.User = url.UserPassword(
		username,
		"[REDACTED]",
	)

	return slog.String(a.Key, parsedURL.String())
}

func initTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	res, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)

	tracer = tracerProvider.Tracer("boot.dev/linko")

	return tracerProvider, nil
}
