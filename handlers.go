package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	
	
	

	"boot.dev/linko/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const shortURLLen = len("http://localhost:8080/") + 6

var (
	redirectsMu sync.Mutex
	redirects   []string
)

//go:embed index.html
var indexPage string



func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(
		r.Context(),
		"handler.login",
	)
	defer span.End()

	r = r.WithContext(ctx)

	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(
		r.Context(),
		"handler.shorten_link",
	)
	defer span.End()

	r = r.WithContext(ctx)

	user, ok := r.Context().Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(
			r.Context(),
			w,
			http.StatusUnauthorized,
			errors.New("unauthorized"),
		)
		return
	}

	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(
			r.Context(),
			w,
			http.StatusBadRequest,
			errors.New("missing url parameter"),
		)
		return
	}

	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(
			r.Context(),
			w,
			http.StatusBadRequest,
			errors.New("invalid URL: must include scheme (http/https) and host"),
		)
		return
	}

	if err := checkDestination(r.Context(), longURL); err != nil {
		httpError(
			r.Context(),
			w,
			http.StatusBadRequest,
			errors.New("invalid target URL"),
		)

		http.Error(w, "invalid target URL", http.StatusBadRequest)
		return
	}

	shortCode, err := s.store.Create(r.Context(), longURL)
	if err != nil {
		s.logger.Error(
			"failed to shorten URL",
			"error", err,
		)

		http.Error(
			w,
			"failed to shorten URL",
			http.StatusInternalServerError,
		)
		return
	}

	s.logger.Info(
		"Successfully generated short code",
		"short_code", shortCode,
		"long_url", longURL,
	)

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	longURL, err := s.store.Lookup(
		r.Context(),
		r.PathValue("shortCode"),
	)

	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(
				r.Context(),
				w,
				http.StatusNotFound,
				store.ErrNotFound,
			)
		} else {
			httpError(
				r.Context(),
				w,
				http.StatusInternalServerError,
				err,
			)
		}

		return
	}

	_, _ = bcrypt.GenerateFromPassword(
		[]byte(longURL),
		bcrypt.DefaultCost,
	)

	if err := checkDestination(r.Context(), longURL); err != nil {
		http.Error(
			w,
			"destination unavailable",
			http.StatusBadGateway,
		)
		return
	}

	redirectsMu.Lock()
	redirects = append(
		redirects,
		strings.Repeat(longURL, 1024),
	)
	redirectsMu.Unlock()

	http.Redirect(
		w,
		r,
		longURL,
		http.StatusFound,
	)
}

func (s *server) handlerListURLs(w http.ResponseWriter, r *http.Request) {
	codes, err := s.store.List(r.Context())
	if err != nil {
		s.logger.Error(
			"failed to list URLs",
			"error", err,
		)

		http.Error(
			w,
			"failed to list URLs",
			http.StatusInternalServerError,
		)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, _ *http.Request) {
	redirectsMu.Lock()
	snapshot := redirects
	redirectsMu.Unlock()

	var bytesSaved int

	for _, u := range snapshot {
		bytesSaved += len(u) - shortURLLen
	}

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]int{
		"redirects":   len(snapshot),
		"bytes_saved": bytesSaved,
	})
}

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(
		r.Context(),
		"handler.index",
	)
	defer span.End()

	r = r.WithContext(ctx)

	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}
