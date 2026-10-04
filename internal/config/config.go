// Package config loads settings from environment variables. Secrets are only
// read from the environment and are never logged.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names.
const (
	EnvProduction  = "production"
	EnvDevelopment = "development"
)

// Config holds runtime settings.
type Config struct {
	Env             string
	HTTPAddr        string
	DatabaseURL     string // secret: may contain credentials
	AutoMigrate     bool
	ProviderTimeout time.Duration
	ShutdownTimeout time.Duration
	LogLevel        slog.Level

	// ExpirySweepInterval is how often overdue CREATED/PENDING payments are
	// expired locally; ExpiryGrace is how long past the provider's
	// expires_at a payment is left alone for late notifications.
	ExpirySweepInterval time.Duration
	ExpiryGrace         time.Duration

	// DevSim enables the development-only provider simulator.
	DevSimEnabled       bool
	DevSimWebhookSecret string // secret

	OVO OVOConfig
}

// OVOConfig configures the OVO SNAP Dynamic QRIS adapter. Values come from
// OVO merchant onboarding.
type OVOConfig struct {
	Enabled        bool
	BaseURL        string
	ClientID       string
	ClientSecret   string // secret
	PrivateKeyFile string // path to the PEM private key (the file content is secret)
	PartnerID      string
	ChannelID      string
	MerchantID     string
	TerminalID     string
	DeviceID       string
	AllowedSources string
	QRExpiry       int // seconds; 0 lets OVO apply its default
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	duration := func(key, def string) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: must be a positive duration", key))
		}
		return d
	}
	boolean := func(key string) bool {
		b, err := strconv.ParseBool(get(key, "false"))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: must be a boolean", key))
		}
		return b
	}

	c := Config{
		Env:                 get("APP_ENV", EnvProduction),
		HTTPAddr:            get("HTTP_ADDR", ":8080"),
		DatabaseURL:         getenv("DATABASE_URL"),
		AutoMigrate:         boolean("AUTO_MIGRATE"),
		ProviderTimeout:     duration("PROVIDER_TIMEOUT", "15s"),
		ShutdownTimeout:     duration("SHUTDOWN_TIMEOUT", "20s"),
		ExpirySweepInterval: duration("EXPIRY_SWEEP_INTERVAL", "1m"),
		ExpiryGrace:         duration("PAYMENT_EXPIRY_GRACE", "5m"),
		DevSimEnabled:       boolean("DEVSIM_ENABLED"),
		DevSimWebhookSecret: getenv("DEVSIM_WEBHOOK_SECRET"),
		OVO: OVOConfig{
			Enabled:        boolean("OVO_ENABLED"),
			BaseURL:        getenv("OVO_BASE_URL"),
			ClientID:       getenv("OVO_CLIENT_ID"),
			ClientSecret:   getenv("OVO_CLIENT_SECRET"),
			PrivateKeyFile: getenv("OVO_PRIVATE_KEY_FILE"),
			PartnerID:      getenv("OVO_PARTNER_ID"),
			ChannelID:      getenv("OVO_CHANNEL_ID"),
			MerchantID:     getenv("OVO_MERCHANT_ID"),
			TerminalID:     getenv("OVO_TERMINAL_ID"),
			DeviceID:       getenv("OVO_DEVICE_ID"),
			AllowedSources: getenv("OVO_ALLOWED_SOURCES"),
		},
	}
	if v := getenv("OVO_QR_EXPIRY_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 75 || n > 86400 {
			errs = append(errs, errors.New("OVO_QR_EXPIRY_SECONDS: must be an integer between 75 and 86400"))
		}
		c.OVO.QRExpiry = n
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "INFO"))); err != nil {
		errs = append(errs, errors.New("LOG_LEVEL: must be DEBUG, INFO, WARN or ERROR"))
	}

	if c.Env != EnvProduction && c.Env != EnvDevelopment {
		errs = append(errs, fmt.Errorf("APP_ENV: must be %q or %q", EnvProduction, EnvDevelopment))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL: required"))
	}
	if c.DevSimEnabled {
		if c.Env == EnvProduction {
			errs = append(errs, errors.New("DEVSIM_ENABLED: the provider simulator cannot be enabled in production"))
		}
		if len(c.DevSimWebhookSecret) < 16 {
			errs = append(errs, errors.New("DEVSIM_WEBHOOK_SECRET: required (at least 16 characters) when DEVSIM_ENABLED=true"))
		}
	}
	if c.OVO.Enabled {
		errs = append(errs, c.OVO.validate(c.Env)...)
		errs = append(errs, validateTZ(getenv("TZ"))...)
	}
	return c, errors.Join(errs...)
}

// validateTZ rejects a TZ that Go cannot load. Go would otherwise silently
// use UTC, and OVO requests must carry the client's actual local time. Unset
// TZ means the system zone; absolute paths are left to the runtime.
func validateTZ(tz string) []error {
	name := strings.TrimPrefix(tz, ":")
	if name == "" || strings.HasPrefix(name, "/") {
		return nil
	}
	if _, err := time.LoadLocation(name); err != nil {
		return []error{fmt.Errorf("TZ: unknown time zone %q", name)}
	}
	return nil
}

// validate checks presence and format. Errors name variables, never values.
// The adapter validates the private key and identifiers again at startup.
func (o OVOConfig) validate(env string) []error {
	var errs []error
	required := []struct{ name, value string }{
		{"OVO_BASE_URL", o.BaseURL}, {"OVO_CLIENT_ID", o.ClientID}, {"OVO_CLIENT_SECRET", o.ClientSecret},
		{"OVO_PRIVATE_KEY_FILE", o.PrivateKeyFile}, {"OVO_PARTNER_ID", o.PartnerID}, {"OVO_CHANNEL_ID", o.ChannelID},
		{"OVO_MERCHANT_ID", o.MerchantID}, {"OVO_TERMINAL_ID", o.TerminalID}, {"OVO_DEVICE_ID", o.DeviceID},
		{"OVO_ALLOWED_SOURCES", o.AllowedSources},
	}
	for _, r := range required {
		if r.value == "" {
			errs = append(errs, fmt.Errorf("%s: required when OVO_ENABLED=true", r.name))
		}
	}
	if o.BaseURL != "" {
		u, err := url.Parse(o.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(env == EnvDevelopment && u.Scheme == "http")) {
			errs = append(errs, errors.New("OVO_BASE_URL: must be an https URL (http is allowed only in development)"))
		}
	}
	return errs
}

// LogValue implements slog.LogValuer so that logging a Config never leaks
// secrets.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", c.Env),
		slog.String("http_addr", c.HTTPAddr),
		slog.Bool("auto_migrate", c.AutoMigrate),
		slog.Duration("provider_timeout", c.ProviderTimeout),
		slog.Duration("expiry_sweep_interval", c.ExpirySweepInterval),
		slog.Duration("payment_expiry_grace", c.ExpiryGrace),
		slog.Bool("devsim_enabled", c.DevSimEnabled),
		slog.Bool("ovo_enabled", c.OVO.Enabled),
		slog.String("ovo_base_url", c.OVO.BaseURL),
		slog.String("log_level", c.LogLevel.String()),
	)
}
