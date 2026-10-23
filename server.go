package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"

	"boot.dev/linko/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

func newServer(
	store store.Store,
	port int,
	cancel context.CancelFunc,
	logger *slog.Logger,
) *server {
	mux := http.NewServeMux()

	otelHandler := otelhttp.NewHandler(
		mux,
		"linko",
	)

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", port),
		Handler: requestID(
			requestLogger(logger)(
				metricsMiddleware(otelHandler)),
		),
	}

	s := &server{
		httpServer: srv,
		store:      store,
		cancel:     cancel,
		logger:     logger,
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /debug/pprof/", s.authMiddleware(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/profile", s.authMiddleware(http.HandlerFunc(pprof.Profile)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port

	s.logger.Debug("Linko is running", "address", fmt.Sprintf("http://localhost:%d", port))

	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	s.logger.Debug("Linko is shutting down")
	return s.httpServer.Shutdown(ctx)
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}

	w.WriteHeader(http.StatusOK)
	go s.cancel()
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Err      error
}

func httpError(
	ctx context.Context,
	w http.ResponseWriter,
	status int,
	err error,
) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Err = err
	}

	message := err.Error()

	switch status {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusInternalServerError:
		message = http.StatusText(status)
	}

	http.Error(w, message, status)

}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			logContext := &LogContext{}

			ctx := context.WithValue(
				r.Context(),
				logContextKey,
				logContext,
			)

			r = r.WithContext(ctx)

			spyReader := &spyReadCloser{
				ReadCloser: r.Body,
			}
			r.Body = spyReader

			spyWriter := &spyResponseWriter{
				ResponseWriter: w,
			}

			next.ServeHTTP(spyWriter, r)

			if spyWriter.statusCode == 0 {
				spyWriter.statusCode = http.StatusOK
			}

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", redactIP(r.RemoteAddr),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				"request_id", r.Header.Get("X-Request-ID"),
			}

			if logContext.Username != "" {
				attrs = append(
					attrs,
					slog.String("user", logContext.Username),
				)
			}

			if logContext.Err != nil {
				attrs = append(
					attrs,
					slog.Any("error", logContext.Err),
				)
			}

			logger.Info(
				"Served request",
				attrs...,
			)
		})
	}
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n

	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")

		if requestID == "" {
			requestID = rand.Text()
		}

		r.Header.Set("X-Request-ID", requestID)
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r)
	})
}

func redactIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		addr = host
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}

	ipv4 := ip.To4()
	if ipv4 == nil {
		return addr
	}

	return fmt.Sprintf(
		"%d.%d.%d.x",
		ipv4[0],
		ipv4[1],
		ipv4[2],
	)
}

var httpRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	},
	[]string{"method", "path", "status"},
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spyWriter := &spyResponseWriter{
			ResponseWriter: w,
		}

		next.ServeHTTP(spyWriter, r)

		statusCode := spyWriter.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}

		httpRequestsTotal.WithLabelValues(
			r.Method,
			r.URL.Path,
			strconv.Itoa(statusCode),
		).Inc()
	})
}
