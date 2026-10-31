// Package server exposes the webhook receiver and read API over HTTP.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/iliyian/sms-receiver/internal/config"
	"github.com/iliyian/sms-receiver/internal/store"
	"github.com/iliyian/sms-receiver/internal/webhook"
)

const (
	maxBodyBytes   = 1 << 20 // 1 MiB
	defaultLimit   = 50
	maxLimit       = 500
	maxQueryLength = 200
)

// Server holds the HTTP dependencies.
type Server struct {
	cfg   config.Config
	store *store.Store
	log   *slog.Logger
}

// New returns the HTTP handler with all routes and middleware wired up.
func New(cfg config.Config, st *store.Store, logger *slog.Logger) http.Handler {
	s := &Server{cfg: cfg, store: st, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /webhook", s.requireToken(s.handleWebhook))
	mux.HandleFunc("GET /api/sms", s.requireToken(s.handleList))
	mux.HandleFunc("GET /api/sms/{id}", s.requireToken(s.handleGet))
	return s.recoverPanic(s.logRequests(mux))
}

type smsItem struct {
	ID           int64      `json:"id"`
	ReceivedAt   time.Time  `json:"received_at"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
	Sender       string     `json:"sender"`
	Content      string     `json:"content"`
	OrgContent   string     `json:"org_content,omitempty"`
	Device       string     `json:"device,omitempty"`
	CardSlot     string     `json:"card_slot,omitempty"`
	Title        string     `json:"title,omitempty"`
	AppVersion   string     `json:"app_version,omitempty"`
	ReceiveTime  string     `json:"receive_time,omitempty"`
	TimestampRaw string     `json:"timestamp_raw,omitempty"`
	ClientIP     string     `json:"client_ip,omitempty"`
	RawBody      string     `json:"raw_body,omitempty"`
	ContentType  string     `json:"content_type,omitempty"`
}

type listResponse struct {
	Items        []smsItem `json:"items"`
	Count        int       `json:"count"`
	NextBeforeID int64     `json:"next_before_id,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("health check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "cannot read request body")
		return
	}

	p := webhook.Parse(r.Header.Get("Content-Type"), body)

	if s.cfg.SignSecret != "" {
		if err := webhook.VerifySign(s.cfg.SignSecret, p.TimestampRaw, p.Sign, time.Now(), s.cfg.SignSkew); err != nil {
			s.log.Warn("webhook rejected: bad signature",
				"ip", clientIP(r, s.cfg.TrustProxy),
				"remote", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
	}

	now := time.Now().UTC()
	rec := store.Record{
		DedupeKey:    webhook.DedupeKey(p, now),
		ReceivedAt:   now,
		SentAt:       p.MessageTime(),
		Sender:       p.Sender,
		Content:      p.Content,
		OrgContent:   p.OrgContent,
		Device:       p.Device,
		CardSlot:     p.CardSlot,
		Title:        p.Title,
		AppVersion:   p.AppVersion,
		ReceiveTime:  p.ReceiveTime,
		TimestampRaw: p.TimestampRaw,
		ClientIP:     clientIP(r, s.cfg.TrustProxy),
		ContentType:  p.ContentType,
		RawBody:      p.RawBody,
	}

	id, duplicate, err := s.store.Insert(r.Context(), rec)
	if err != nil {
		s.log.Error("persist webhook failed", "err", err)
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
		return
	}

	s.log.Info("sms stored", "id", id, "content_len", len(p.Content))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "id": id})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ListFilter{
		Limit:  defaultLimit,
		Sender: strings.TrimSpace(q.Get("from")),
		Device: strings.TrimSpace(q.Get("device")),
		Query:  strings.TrimSpace(q.Get("q")),
	}

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 500")
			return
		}
		filter.Limit = n
	}
	if v := q.Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "before_id must be a positive integer")
			return
		}
		filter.BeforeID = n
	}
	if len(filter.Query) > maxQueryLength {
		writeError(w, http.StatusBadRequest, "q is too long")
		return
	}
	if v := q.Get("since"); v != "" {
		t, ok := webhook.ParseTime(v)
		if !ok {
			writeError(w, http.StatusBadRequest, "since must be RFC3339 or a Unix timestamp")
			return
		}
		filter.Since = &t
	}
	if v := q.Get("until"); v != "" {
		t, ok := webhook.ParseTime(v)
		if !ok {
			writeError(w, http.StatusBadRequest, "until must be RFC3339 or a Unix timestamp")
			return
		}
		filter.Until = &t
	}
	if filter.Since != nil && filter.Until != nil && filter.Since.After(*filter.Until) {
		writeError(w, http.StatusBadRequest, "since must be before until")
		return
	}

	records, err := s.store.List(r.Context(), filter)
	if err != nil {
		s.log.Error("list sms failed", "err", err)
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}

	resp := listResponse{Items: make([]smsItem, 0, len(records)), Count: len(records)}
	for _, rec := range records {
		resp.Items = append(resp.Items, toItem(rec))
	}
	if len(records) == filter.Limit {
		resp.NextBeforeID = records[len(records)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	rec, err := s.store.Get(r.Context(), id)
	if err != nil {
		s.log.Error("get sms failed", "err", err)
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	item := toItem(*rec)
	item.RawBody = rec.RawBody
	item.ContentType = rec.ContentType
	writeJSON(w, http.StatusOK, item)
}

func toItem(rec store.Record) smsItem {
	return smsItem{
		ID:           rec.ID,
		ReceivedAt:   rec.ReceivedAt,
		SentAt:       rec.SentAt,
		Sender:       rec.Sender,
		Content:      rec.Content,
		OrgContent:   rec.OrgContent,
		Device:       rec.Device,
		CardSlot:     rec.CardSlot,
		Title:        rec.Title,
		AppVersion:   rec.AppVersion,
		ReceiveTime:  rec.ReceiveTime,
		TimestampRaw: rec.TimestampRaw,
		ClientIP:     rec.ClientIP,
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r, s.cfg.TrustProxy))
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic while handling request",
					"panic", rec,
					"path", r.URL.Path,
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			token = r.Header.Get("X-Webhook-Token")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if !secureEqual(token, s.cfg.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sms-receiver"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

// secureEqual compares SHA-256 digests so the comparison is constant time and
// independent of input length.
func secureEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
		if xr := r.Header.Get("X-Real-IP"); strings.TrimSpace(xr) != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
