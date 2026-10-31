package server_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iliyian/sms-receiver/internal/config"
	"github.com/iliyian/sms-receiver/internal/server"
	"github.com/iliyian/sms-receiver/internal/store"
)

const testToken = "test-token"

func newHandler(t *testing.T, signSecret string) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		DBPath:     "unused",
		Token:      testToken,
		SignSecret: signSecret,
		SignSkew:   time.Hour,
		LogLevel:   "error",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return server.New(cfg, st, logger)
}

func request(t *testing.T, h http.Handler, method, target, body, contentType, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type webhookResponse struct {
	Status string `json:"status"`
	ID     int64  `json:"id"`
}

type item struct {
	ID           int64      `json:"id"`
	ReceivedAt   time.Time  `json:"received_at"`
	SentAt       *time.Time `json:"sent_at"`
	Sender       string     `json:"sender"`
	Content      string     `json:"content"`
	Device       string     `json:"device"`
	CardSlot     string     `json:"card_slot"`
	ReceiveTime  string     `json:"receive_time"`
	TimestampRaw string     `json:"timestamp_raw"`
	RawBody      string     `json:"raw_body"`
}

type listResponse struct {
	Items        []item `json:"items"`
	Count        int    `json:"count"`
	NextBeforeID int64  `json:"next_before_id"`
}

func TestWebhookAuthAndPersistence(t *testing.T) {
	h := newHandler(t, "")

	if rec := request(t, h, http.MethodGet, "/healthz", "", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}

	form := url.Values{
		"from":      {"15888888888"},
		"content":   {"您的验证码是 654321"},
		"timestamp": {"1712345678901"},
	}
	body := form.Encode()

	if rec := request(t, h, http.MethodPost, "/webhook", body, "application/x-www-form-urlencoded", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}
	if rec := request(t, h, http.MethodPost, "/webhook", body, "application/x-www-form-urlencoded", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rec.Code)
	}

	rec := request(t, h, http.MethodPost, "/webhook", body, "application/x-www-form-urlencoded", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var wh webhookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wh); err != nil {
		t.Fatalf("decode webhook response: %v", err)
	}
	if wh.Status != "ok" || wh.ID != 1 {
		t.Fatalf("webhook response = %+v", wh)
	}

	// Same request again must be treated as a duplicate, not a second row.
	rec = request(t, h, http.MethodPost, "/webhook", body, "application/x-www-form-urlencoded", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wh); err != nil {
		t.Fatal(err)
	}
	if wh.Status != "duplicate" {
		t.Fatalf("duplicate response = %+v", wh)
	}

	// Query-string token works for SmsForwarder's WebServer URL style.
	rec = request(t, h, http.MethodGet, "/api/sms?token="+testToken, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Count != 1 || len(list.Items) != 1 {
		t.Fatalf("list = %+v", list)
	}
	got := list.Items[0]
	if got.Sender != "15888888888" || got.Content != "您的验证码是 654321" {
		t.Fatalf("stored item = %+v", got)
	}
	if got.SentAt == nil || got.SentAt.UnixMilli() != 1712345678901 {
		t.Fatalf("sent_at = %v, want 1712345678901ms", got.SentAt)
	}

	// Detail includes the raw body.
	rec = request(t, h, http.MethodGet, "/api/sms/1", "", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var detail item
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RawBody != body {
		t.Fatalf("raw body = %q, want %q", detail.RawBody, body)
	}

	if rec := request(t, h, http.MethodGet, "/api/sms/999", "", "", testToken); rec.Code != http.StatusNotFound {
		t.Fatalf("missing record status = %d, want 404", rec.Code)
	}
}

func TestWebhookJSONTemplateAndFilters(t *testing.T) {
	h := newHandler(t, "")

	body := `{"from":"10086","content":"验证码 111111","timestamp":"1712345678902",` +
		`"device_mark":"Pixel 8","card_slot":"卡1","receive_time":"2024-04-05 20:34:38"}`
	rec := request(t, h, http.MethodPost, "/webhook", body, "application/json", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, body = %s", rec.Code, rec.Body.String())
	}

	other := url.Values{
		"from":      {"95588"},
		"content":   {"消费提醒 100 元"},
		"timestamp": {"1712345678903"},
	}
	if rec := request(t, h, http.MethodPost, "/webhook", other.Encode(), "application/x-www-form-urlencoded", testToken); rec.Code != http.StatusOK {
		t.Fatalf("second webhook status = %d", rec.Code)
	}

	get := func(query string) listResponse {
		t.Helper()
		rec := request(t, h, http.MethodGet, "/api/sms?"+query, "", "", testToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %q status = %d, body = %s", query, rec.Code, rec.Body.String())
		}
		var lr listResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
			t.Fatal(err)
		}
		return lr
	}

	if lr := get("from=10086"); lr.Count != 1 || lr.Items[0].Device != "Pixel 8" || lr.Items[0].CardSlot != "卡1" {
		t.Fatalf("from filter = %+v", lr)
	}
	if lr := get("q=" + url.QueryEscape("验证码")); lr.Count != 1 || lr.Items[0].Sender != "10086" {
		t.Fatalf("q filter = %+v", lr)
	}
	if lr := get("limit=1"); lr.Count != 1 || lr.NextBeforeID == 0 {
		t.Fatalf("limit filter = %+v", lr)
	}
	if lr := get("before_id=2"); lr.Count != 1 || lr.Items[0].ID != 1 {
		t.Fatalf("before_id filter = %+v", lr)
	}
	since := time.Now().Add(-time.Hour).Format(time.RFC3339)
	until := time.Now().Add(time.Hour).Format(time.RFC3339)
	if lr := get("since=" + url.QueryEscape(since) + "&until=" + url.QueryEscape(until)); lr.Count != 2 {
		t.Fatalf("time filter = %+v", lr)
	}
	if rec := request(t, h, http.MethodGet, "/api/sms?limit=0", "", "", testToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit status = %d", rec.Code)
	}
}

func TestWebhookSignVerification(t *testing.T) {
	const secret = "app-secret"
	h := newHandler(t, secret)

	now := time.Now()
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Missing sign is rejected when a secret is configured.
	noSign := url.Values{"from": {"10086"}, "content": {"hi"}, "timestamp": {ts}}
	if rec := request(t, h, http.MethodPost, "/webhook", noSign.Encode(), "application/x-www-form-urlencoded", testToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing sign status = %d, want 401", rec.Code)
	}

	good := url.Values{"from": {"10086"}, "content": {"hi"}, "timestamp": {ts}, "sign": {sign}}
	if rec := request(t, h, http.MethodPost, "/webhook", good.Encode(), "application/x-www-form-urlencoded", testToken); rec.Code != http.StatusOK {
		t.Fatalf("valid sign status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A timestamp outside the allowed window is rejected.
	oldTS := strconv.FormatInt(now.Add(-2*time.Hour).UnixMilli(), 10)
	oldMac := hmac.New(sha256.New, []byte(secret))
	oldMac.Write([]byte(oldTS + "\n" + secret))
	oldSign := base64.StdEncoding.EncodeToString(oldMac.Sum(nil))
	stale := url.Values{"from": {"10086"}, "content": {"hi"}, "timestamp": {oldTS}, "sign": {oldSign}}
	if rec := request(t, h, http.MethodPost, "/webhook", stale.Encode(), "application/x-www-form-urlencoded", testToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale sign status = %d, want 401", rec.Code)
	}

	// JSON templates carry the sign as a normal field.
	jsonBody := `{"from":"10086","content":"from json","timestamp":"` + ts + `","sign":"` + sign + `"}`
	if rec := request(t, h, http.MethodPost, "/webhook", jsonBody, "application/json", testToken); rec.Code != http.StatusOK {
		t.Fatalf("json sign status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
