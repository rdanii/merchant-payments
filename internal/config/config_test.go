package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(map[string]string{"DATABASE_URL": "postgres://u:p@h/db"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != EnvProduction || c.HTTPAddr != ":8080" || c.DevSimEnabled {
		t.Fatalf("unexpected defaults %+v", c)
	}
	if c.ExpirySweepInterval != time.Minute || c.ExpiryGrace != 5*time.Minute {
		t.Fatalf("unexpected expiry defaults %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing database url", map[string]string{}, "DATABASE_URL"},
		{"bad timeout", map[string]string{"DATABASE_URL": "x", "PROVIDER_TIMEOUT": "soon"}, "PROVIDER_TIMEOUT"},
		{"bad sweep interval", map[string]string{"DATABASE_URL": "x", "EXPIRY_SWEEP_INTERVAL": "0s"}, "EXPIRY_SWEEP_INTERVAL"},
		{"bad expiry grace", map[string]string{"DATABASE_URL": "x", "PAYMENT_EXPIRY_GRACE": "-1m"}, "PAYMENT_EXPIRY_GRACE"},
		{"simulator in production", map[string]string{"DATABASE_URL": "x", "DEVSIM_ENABLED": "true",
			"DEVSIM_WEBHOOK_SECRET": "0123456789abcdef"}, "cannot be enabled in production"},
		{"simulator without secret", map[string]string{"DATABASE_URL": "x", "APP_ENV": "development",
			"DEVSIM_ENABLED": "true"}, "DEVSIM_WEBHOOK_SECRET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	c := Config{DatabaseURL: "postgres://user:hunter2@db/x", DevSimWebhookSecret: "supersecretvalue"}
	s := c.LogValue().String()
	if strings.Contains(s, "hunter2") || strings.Contains(s, "supersecretvalue") {
		t.Fatalf("secrets leaked into log value: %s", s)
	}
}

func ovoEnv(extra map[string]string) map[string]string {
	m := map[string]string{
		"DATABASE_URL": "x", "OVO_ENABLED": "true", "OVO_BASE_URL": "https://ovo.example",
		"OVO_CLIENT_ID": "cid", "OVO_CLIENT_SECRET": "ovo-secret-value", "OVO_PRIVATE_KEY_FILE": "/run/secrets/ovo.pem",
		"OVO_PARTNER_ID": "pid", "OVO_CHANNEL_ID": "ch", "OVO_MERCHANT_ID": "mid", "OVO_TERMINAL_ID": "tid",
		"OVO_DEVICE_ID": "dev", "OVO_ALLOWED_SOURCES": "100",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadOVO(t *testing.T) {
	c, err := load(env(ovoEnv(map[string]string{"OVO_QR_EXPIRY_SECONDS": "300"})))
	if err != nil {
		t.Fatal(err)
	}
	if !c.OVO.Enabled || c.OVO.ClientSecret != "ovo-secret-value" || c.OVO.QRExpiry != 300 {
		t.Fatalf("unexpected OVO config %+v", c.OVO)
	}

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing secret", ovoEnv(map[string]string{"OVO_CLIENT_SECRET": ""}), "OVO_CLIENT_SECRET"},
		{"missing device", ovoEnv(map[string]string{"OVO_DEVICE_ID": ""}), "OVO_DEVICE_ID"},
		{"http in production", ovoEnv(map[string]string{"OVO_BASE_URL": "http://ovo.example"}), "OVO_BASE_URL"},
		{"expiry too short", ovoEnv(map[string]string{"OVO_QR_EXPIRY_SECONDS": "74"}), "OVO_QR_EXPIRY_SECONDS"},
		{"expiry not a number", ovoEnv(map[string]string{"OVO_QR_EXPIRY_SECONDS": "1m"}), "OVO_QR_EXPIRY_SECONDS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error about %s, got %v", tt.want, err)
			}
			if strings.Contains(err.Error(), "ovo-secret-value") {
				t.Fatalf("error leaks the secret: %v", err)
			}
		})
	}

	dev := ovoEnv(map[string]string{"APP_ENV": "development", "OVO_BASE_URL": "http://localhost:9999"})
	if _, err := load(env(dev)); err != nil {
		t.Fatalf("http base URL must be allowed in development: %v", err)
	}
	disabled := ovoEnv(map[string]string{"OVO_ENABLED": "false", "OVO_CLIENT_SECRET": ""})
	if _, err := load(env(disabled)); err != nil {
		t.Fatalf("disabled OVO must not be validated: %v", err)
	}
}

func TestLogValueRedactsOVOSecrets(t *testing.T) {
	c, err := load(env(ovoEnv(nil)))
	if err != nil {
		t.Fatal(err)
	}
	s := c.LogValue().String()
	if strings.Contains(s, "ovo-secret-value") || strings.Contains(s, "/run/secrets/ovo.pem") {
		t.Fatalf("OVO secrets leaked into log value: %s", s)
	}
	if !strings.Contains(s, "ovo_enabled=true") {
		t.Fatalf("log value should show that OVO is enabled: %s", s)
	}
}

func TestOVOValidatesTimeZone(t *testing.T) {
	for _, tz := range []string{"", "Asia/Jakarta", ":Asia/Jakarta", "UTC", "/etc/localtime"} {
		if _, err := load(env(ovoEnv(map[string]string{"TZ": tz}))); err != nil {
			t.Errorf("TZ=%q rejected: %v", tz, err)
		}
	}
	for _, tz := range []string{"Asia/Jakart", "Mars/Olympus_Mons", "UTC+7"} {
		_, err := load(env(ovoEnv(map[string]string{"TZ": tz})))
		if err == nil || !strings.Contains(err.Error(), "TZ: unknown time zone") {
			t.Errorf("TZ=%q: want rejection (Go would silently use UTC), got %v", tz, err)
		}
	}
	// Without OVO the time zone is not an OVO concern and is not validated.
	if _, err := load(env(map[string]string{"DATABASE_URL": "x", "TZ": "Asia/Jakart"})); err != nil {
		t.Errorf("TZ must only be validated when OVO is enabled: %v", err)
	}
}
