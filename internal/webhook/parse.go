// Package webhook parses SmsForwarder webhook payloads and verifies their
// optional HMAC signature.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Payload is the normalized form of a webhook request body. Unknown template
// keys are ignored, but the complete body is always kept in RawBody.
type Payload struct {
	Sender       string
	Content      string
	OrgContent   string
	TimestampRaw string
	ReceiveTime  string
	Device       string
	CardSlot     string
	Title        string
	AppVersion   string
	Sign         string

	RawBody     string
	ContentType string
}

// Parse accepts the three shapes SmsForwarder can send: a JSON object
// (custom template), an urlencoded form (default params or custom template),
// or arbitrary text. Anything unrecognized is stored as Content.
func Parse(contentType string, body []byte) Payload {
	p := Payload{RawBody: string(body), ContentType: contentType}
	trimmed := bytes.TrimSpace(body)

	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&obj); err == nil {
			index := make(map[string]string, len(obj))
			for k, v := range obj {
				index[canonicalKey(k)] = jsonScalar(v)
			}
			p.fillFrom(index)
			return p
		}
	}

	if vals, err := url.ParseQuery(string(body)); err == nil && len(vals) > 0 {
		index := make(map[string]string, len(vals))
		for k, list := range vals {
			if len(list) > 0 {
				index[canonicalKey(k)] = list[0]
			}
		}
		if looksLikePayload(index) {
			p.fillFrom(index)
			return p
		}
	}

	// Not JSON or a recognizable form: keep the text as the message content.
	p.Content = string(body)
	return p
}

var (
	senderAliases     = []string{"from", "sender", "origin", "phone", "number", "source"}
	contentAliases    = []string{"content", "msg", "message", "text", "sms", "body"}
	orgContentAliases = []string{"orgcontent", "originalcontent", "origincontent"}
	timestampAliases  = []string{"timestamp", "ts", "time", "sentat"}
	receiveAliases    = []string{"receivetime", "receivedtime"}
	deviceAliases     = []string{"devicemark", "devicename", "device"}
	cardSlotAliases   = []string{"cardslot", "slot", "sim", "simslot"}
	titleAliases      = []string{"title", "subject"}
	appVersionAliases = []string{"appversion", "version"}
	signAliases       = []string{"sign", "signature"}
)

func (p *Payload) fillFrom(index map[string]string) {
	p.Sender = firstNonEmpty(index, senderAliases...)
	p.Content = firstNonEmpty(index, contentAliases...)
	p.OrgContent = firstNonEmpty(index, orgContentAliases...)
	p.TimestampRaw = firstNonEmpty(index, timestampAliases...)
	p.ReceiveTime = firstNonEmpty(index, receiveAliases...)
	p.Device = firstNonEmpty(index, deviceAliases...)
	p.CardSlot = firstNonEmpty(index, cardSlotAliases...)
	p.Title = firstNonEmpty(index, titleAliases...)
	p.AppVersion = firstNonEmpty(index, appVersionAliases...)
	p.Sign = firstNonEmpty(index, signAliases...)
}

func looksLikePayload(index map[string]string) bool {
	for _, key := range []string{"from", "sender", "content", "msg", "message", "text", "sms", "timestamp", "orgcontent", "devicemark"} {
		if _, ok := index[key]; ok {
			return true
		}
	}
	return false
}

func firstNonEmpty(index map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := index[k]; v != "" {
			return v
		}
	}
	return ""
}

// canonicalKey lowercases and strips separators so that "device_mark",
// "deviceMark" and "Device-Mark" all map to the same field.
func canonicalKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(s)
}

func jsonScalar(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return ""
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

// MessageTime returns the best-effort time the message was received on the
// phone: receive_time if parseable, otherwise the millisecond timestamp.
func (p Payload) MessageTime() *time.Time {
	if t, ok := ParseTime(p.ReceiveTime); ok {
		return &t
	}
	if t, ok := ParseTime(p.TimestampRaw); ok {
		return &t
	}
	return nil
}

// ParseTime accepts Unix seconds/milliseconds/microseconds/nanoseconds as well
// as common date-time layouts. The result is always UTC.
func ParseTime(raw string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}

	if isDigits(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			var t time.Time
			switch {
			case len(s) <= 10:
				t = time.Unix(n, 0)
			case len(s) <= 13:
				t = time.UnixMilli(n)
			case len(s) <= 16:
				t = time.UnixMicro(n)
			default:
				t = time.Unix(0, n)
			}
			t = t.UTC()
			if t.Year() >= 2000 && t.Year() <= 2100 {
				return t, true
			}
			return time.Time{}, false
		}
	}

	// Numeric with a decimal fraction is treated as Unix seconds.
	if t, err := strconv.ParseFloat(s, 64); err == nil && strings.Contains(s, ".") {
		tt := time.Unix(0, int64(t*float64(time.Second))).UTC()
		if tt.Year() >= 2000 && tt.Year() <= 2100 {
			return tt, true
		}
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006/01/02 15:04:05",
		"2006-01-02 15:04:05.000",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			t = t.UTC()
			if t.Year() >= 2000 && t.Year() <= 2100 {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// DedupeKey derives a stable key for one forward attempt. Retries of the same
// webhook produce the same key; when neither timestamp nor receive_time is
// present, identical messages are considered duplicates within a 5 minute
// window.
func DedupeKey(p Payload, now time.Time) string {
	h := sha256.New()
	for _, field := range []string{p.Device, p.Sender, p.Content, p.OrgContent, p.TimestampRaw, p.ReceiveTime} {
		fmt.Fprintf(h, "%d:%s\n", len(field), field)
	}
	if p.TimestampRaw == "" && p.ReceiveTime == "" {
		fmt.Fprintf(h, "bucket:%d\n", now.Unix()/300)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ErrBadSignature is returned for any signature problem. Details are
// deliberately not exposed to clients.
var ErrBadSignature = errors.New("invalid signature")

// VerifySign implements SmsForwarder's sign algorithm:
// base64(HMAC-SHA256(timestamp+"\n"+secret)), with a timestamp freshness check.
func VerifySign(secret, timestampRaw, sign string, now time.Time, skew time.Duration) error {
	timestampRaw = strings.TrimSpace(timestampRaw)
	sign = strings.TrimSpace(sign)
	if timestampRaw == "" || sign == "" {
		return ErrBadSignature
	}
	ts, ok := ParseTime(timestampRaw)
	if !ok {
		return ErrBadSignature
	}
	diff := now.Sub(ts)
	if diff < 0 {
		diff = -diff
	}
	if diff > skew {
		return ErrBadSignature
	}

	provided, ok := decodeSign(sign)
	if !ok {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestampRaw + "\n" + secret))
	if !hmac.Equal(mac.Sum(nil), provided) {
		return ErrBadSignature
	}
	return nil
}

// decodeSign tolerates raw base64, urlencoded base64 and base64url because the
// app's template substitution may or may not URL-encode the value.
func decodeSign(sign string) ([]byte, bool) {
	candidates := []string{sign}
	if unescaped, err := url.QueryUnescape(sign); err == nil && unescaped != sign {
		candidates = append(candidates, unescaped)
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, candidate := range candidates {
		for _, enc := range encodings {
			if b, err := enc.DecodeString(candidate); err == nil && len(b) > 0 {
				return b, true
			}
		}
	}
	return nil, false
}
