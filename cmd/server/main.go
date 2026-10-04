// Command server runs the merchant payments HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embedded zone database: TZ names resolve even in images without
	// /usr/share/zoneinfo, instead of Go silently falling back to UTC.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"

	"merchant-payments/internal/config"
	"merchant-payments/internal/httpapi"
	"merchant-payments/internal/payment"
	"merchant-payments/internal/provider"
	"merchant-payments/internal/provider/devsim"
	"merchant-payments/internal/provider/ovo"
	"merchant-payments/internal/requestid"
	"merchant-payments/internal/store/postgres"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// Used by container healthchecks: the runtime image has no shell or
		// curl. Exits non-zero unless GET /healthz returns 200.
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(requestid.Handler{Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})})
	slog.SetDefault(log)
	log.Info("starting", "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid DATABASE_URL") // the raw error may echo the URL
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	if cfg.AutoMigrate {
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
		log.Info("migrations applied")
	}

	providers, err := buildProviders(cfg, log)
	if err != nil {
		return err
	}

	svc := payment.NewService(postgres.New(pool), providers, cfg.ProviderTimeout, log)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(svc, pool.Ping, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Must exceed PROVIDER_TIMEOUT so payment creation can respond.
		WriteTimeout: cfg.ProviderTimeout + 15*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go runExpirySweeper(ctx, svc, cfg, log)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// runExpirySweeper periodically expires payments whose provider-issued
// expires_at has passed, until ctx is cancelled. Running it on several
// instances is safe: each expiry re-checks the payment under a row lock.
func runExpirySweeper(ctx context.Context, svc *payment.Service, cfg config.Config, log *slog.Logger) {
	ticker := time.NewTicker(cfg.ExpirySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := svc.ExpireOverduePayments(ctx, cfg.ExpiryGrace)
			if err != nil && ctx.Err() == nil {
				log.Error("payment expiry sweep failed", "error", err)
			}
			if n > 0 {
				log.Info("payment expiry sweep", "expired", n)
			}
		}
	}
}

func healthcheck() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// buildProviders registers the provider adapters enabled by configuration.
//
// OVO SNAP Dynamic QRIS is the only production adapter (inquiry-confirmed,
// no callbacks). Other providers need adapters built from their official API
// documentation; register them here once they exist (see docs/providers.md).
func buildProviders(cfg config.Config, log *slog.Logger) (*provider.Registry, error) {
	var adapters []provider.Adapter
	if cfg.OVO.Enabled {
		keyPEM, err := os.ReadFile(cfg.OVO.PrivateKeyFile)
		if err != nil {
			return nil, errors.New("OVO_PRIVATE_KEY_FILE: cannot read the private key file")
		}
		adapter, err := ovo.New(ovo.Config{
			BaseURL:         cfg.OVO.BaseURL,
			ClientID:        cfg.OVO.ClientID,
			ClientSecret:    cfg.OVO.ClientSecret,
			PrivateKeyPEM:   keyPEM,
			PartnerID:       cfg.OVO.PartnerID,
			ChannelID:       cfg.OVO.ChannelID,
			MerchantID:      cfg.OVO.MerchantID,
			TerminalID:      cfg.OVO.TerminalID,
			DeviceID:        cfg.OVO.DeviceID,
			AllowedSources:  cfg.OVO.AllowedSources,
			QRExpirySeconds: cfg.OVO.QRExpiry,
			Location:        time.Local, // the runtime zone (TZ), validated by config.Load
		})
		if err != nil {
			return nil, err
		}
		adapters = append(adapters, adapter)
		log.Info("provider enabled", "provider", ovo.Name, "confirmation", "inquiry", "callbacks", "unsupported",
			"local_time_zone", time.Local.String())
	}
	if cfg.DevSimEnabled {
		sim, err := devsim.New(cfg.DevSimWebhookSecret)
		if err != nil {
			return nil, err
		}
		adapters = append(adapters, sim)
		log.Warn("DEVELOPMENT provider simulator enabled; it does not process real payments", "provider", devsim.Name)
	}
	reg, err := provider.NewRegistry(adapters...)
	if err != nil {
		return nil, err
	}
	if len(reg.Names()) == 0 {
		log.Warn("no payment providers enabled; payment creation will be rejected")
	}
	return reg, nil
}
