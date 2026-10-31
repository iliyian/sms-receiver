// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the complete runtime configuration for the server.
type Config struct {
	// ListenAddr is the HTTP listen address, e.g. "127.0.0.1:8044".
	ListenAddr string
	// DBPath is the SQLite database file path.
	DBPath string
	// Token is the shared secret required on every API request.
	Token string
	// SignSecret, when non-empty, enables SmsForwarder HMAC-SHA256 signature
	// verification (the "secret" configured in the app's webhook settings).
	SignSecret string
	// SignSkew is the maximum accepted clock difference for signed requests.
	SignSkew time.Duration
	// TrustProxy makes the server read the client IP from X-Forwarded-For.
	TrustProxy bool
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Load reads configuration from the environment. It fails fast on missing
// required values or malformed settings.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr: env("LISTEN_ADDR", "127.0.0.1:8044"),
		DBPath:     env("DB_PATH", "data/sms.db"),
		Token:      os.Getenv("WEBHOOK_TOKEN"),
		SignSecret: os.Getenv("SMSF_SECRET"),
		SignSkew:   time.Hour,
		LogLevel:   env("LOG_LEVEL", "info"),
	}

	if cfg.Token == "" {
		return Config{}, errors.New("WEBHOOK_TOKEN is required (set it to a long random string)")
	}

	if v := os.Getenv("SMSF_SIGN_SKEW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SMSF_SIGN_SKEW must be a positive duration, got %q", v)
		}
		cfg.SignSkew = d
	}

	if v := os.Getenv("TRUST_PROXY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("TRUST_PROXY must be true or false, got %q", v)
		}
		cfg.TrustProxy = b
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", cfg.LogLevel)
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
