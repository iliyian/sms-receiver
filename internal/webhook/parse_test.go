package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestParseDefaultForm(t *testing.T) {
	body := "from=15888888888&content=123456&timestamp=1712345678901&sign=abc"
	p := Parse("application/x-www-form-urlencoded", []byte(body))

	if p.Sender != "15888888888" {
		t.Errorf("Sender = %q, want 15888888888", p.Sender)
	}
	if p.Content != "123456" {
		t.Errorf("Content = %q, want 123456", p.Content)
	}
	if p.TimestampRaw != "1712345678901" {
		t.Errorf("TimestampRaw = %q", p.TimestampRaw)
	}
	if p.Sign != "abc" {
		t.Errorf("Sign = %q", p.Sign)
	}
	if p.RawBody != body {
		t.Errorf("RawBody was not preserved")
	}
}

func TestParseJSONTemplate(t *testing.T) {
	body := `{"from":"10086","content":"你的验证码是 123456","timestamp":"1712345678901",` +
		`"device_mark":"Pixel 8","card_slot":"卡1","receive_time":"2024-04-05 20:34:38",` +
		`"org_content":"原始内容","app_version":"3.3.3","sign":"abc"}`

	p := Parse("application/json;charset=utf-8", []byte(body))

	if p.Sender != "10086" || p.Content != "你的验证码是 123456" {
		t.Errorf("unexpected sender/content: %q / %q", p.Sender, p.Content)
	}
	if p.Device != "Pixel 8" || p.CardSlot != "卡1" {
		t.Errorf("unexpected device/card slot: %q / %q", p.Device, p.CardSlot)
	}
	if p.ReceiveTime != "2024-04-05 20:34:38" || p.OrgContent != "原始内容" {
		t.Errorf("unexpected receive time/org content: %q / %q", p.ReceiveTime, p.OrgContent)
	}
	if p.AppVersion != "3.3.3" || p.Sign != "abc" {
		t.Errorf("unexpected app version/sign: %q / %q", p.AppVersion, p.Sign)
	}
}

func TestParseJSONAliases(t *testing.T) {
	p := Parse("application/json", []byte(`{"sender":"95588","msg":"hello","deviceName":"phone-a"}`))

	if p.Sender != "95588" {
		t.Errorf("Sender = %q", p.Sender)
	}
	if p.Content != "hello" {
		t.Errorf("Content = %q", p.Content)
	}
	if p.Device != "phone-a" {
		t.Errorf("Device = %q", p.Device)
	}
}

func TestParsePlainText(t *testing.T) {
	p := Parse("text/plain", []byte("just some text"))
	if p.Content != "just some text" {
		t.Errorf("Content = %q", p.Content)
	}
	if p.Sender != "" {
		t.Errorf("Sender = %q, want empty", p.Sender)
	}
}

func TestParseTime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"1712345678901", time.UnixMilli(1712345678901).UTC()},
		{"1712345678", time.Unix(1712345678, 0).UTC()},
		{"2024-04-05T20:34:38+08:00", time.Date(2024, 4, 5, 12, 34, 38, 0, time.UTC)},
		{"2024-04-05 20:34:38", time.Date(2024, 4, 5, 20, 34, 38, 0, time.Local).UTC()},
	}
	for _, tc := range cases {
		got, ok := ParseTime(tc.in)
		if !ok {
			t.Errorf("ParseTime(%q) failed", tc.in)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "hello", "123", "99999999999999999999999"} {
		if _, ok := ParseTime(bad); ok {
			t.Errorf("ParseTime(%q) should fail", bad)
		}
	}
}

func TestDedupeKey(t *testing.T) {
	now := time.Unix(1700000100, 0)
	p := Payload{Sender: "10086", Content: "hi", TimestampRaw: "1700000000000"}

	if DedupeKey(p, now) != DedupeKey(p, now.Add(time.Hour)) {
		t.Error("same timestamped payload should have a stable key")
	}

	p2 := p
	p2.TimestampRaw = "1700000000001"
	if DedupeKey(p, now) == DedupeKey(p2, now) {
		t.Error("different timestamps should produce different keys")
	}

	// Without a timestamp, identical messages dedupe inside the same 5 minute
	// bucket but not across buckets.
	noTS := Payload{Sender: "10086", Content: "hi"}
	if DedupeKey(noTS, now) != DedupeKey(noTS, now.Add(2*time.Minute)) {
		t.Error("same bucket should dedupe")
	}
	if DedupeKey(noTS, now) == DedupeKey(noTS, now.Add(10*time.Minute)) {
		t.Error("different buckets should not dedupe")
	}
}

func TestVerifySign(t *testing.T) {
	secret := "test-secret"
	now := time.Unix(1712345678, 0)
	ts := strconv.FormatInt(now.UnixMilli(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if err := VerifySign(secret, ts, sign, now, time.Hour); err != nil {
		t.Fatalf("valid sign rejected: %v", err)
	}
	if err := VerifySign(secret, ts, url.QueryEscape(sign), now, time.Hour); err != nil {
		t.Fatalf("urlencoded sign rejected: %v", err)
	}
	if err := VerifySign(secret, ts, sign, now.Add(2*time.Hour), time.Hour); err == nil {
		t.Error("stale timestamp accepted")
	}
	if err := VerifySign(secret, ts, "bad", now, time.Hour); err == nil {
		t.Error("bad sign accepted")
	}
	if err := VerifySign(secret, "", "", now, time.Hour); err == nil {
		t.Error("missing sign accepted")
	}
	if err := VerifySign("other-secret", ts, sign, now, time.Hour); err == nil {
		t.Error("signature from another secret accepted")
	}
}
