package app

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/100bench/OPORA/internal/domain"
	"github.com/100bench/OPORA/internal/infrastructure"
	"github.com/100bench/OPORA/internal/infrastructure/auth"
	runtimeconfig "github.com/100bench/OPORA/internal/infrastructure/config"
	"github.com/100bench/OPORA/internal/infrastructure/openai"
	"github.com/100bench/OPORA/internal/service"
)

const shutdownTimeout = 10 * time.Second

// Runtime owns the configured HTTP server lifecycle.
type Runtime struct {
	server *http.Server
}

// NewRuntime composes authentication, provider, service, router, and server.
func NewRuntime(getenv func(string) string, logger *slog.Logger) (*Runtime, error) {
	cfg, err := runtimeconfig.Load(getenv)
	if err != nil {
		return nil, runtimeconfig.ErrInvalidConfiguration
	}
	authenticator, err := auth.NewDeviceTokens(cfg.DeviceTokensJSON)
	if err != nil {
		return nil, auth.ErrInvalidConfiguration
	}
	if logger == nil {
		logger = slog.Default()
	}
	httpClient := &http.Client{Timeout: 20 * time.Second}
	provider := openai.NewWithConfig(openai.Config{
		Endpoint:           openai.DefaultEndpoint,
		APIKey:             cfg.OpenAIAPIKey,
		PlannerModel:       cfg.PlannerModel,
		TranscriptionModel: cfg.TranscriptionModel,
		HTTPClient:         httpClient,
		Timeout:            20 * time.Second,
	})
	assistant := service.New(provider, provider, infrastructure.SystemClock{}, service.DefaultConfig())
	readiness := func(ctx context.Context) error {
		if err := provider.Ready(); err != nil {
			return &domain.AppError{Code: domain.CodeProviderUnavailable, Cause: err}
		}
		return assistant.Ready(ctx)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           NewRouter(Dependencies{Assistant: assistant, Auth: authenticator, Logger: logger, Readiness: readiness}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      25 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return &Runtime{server: server}, nil
}

// Handler returns the configured HTTP handler for local verification.
func (r *Runtime) Handler() http.Handler { return r.server.Handler }

// Server returns the configured server for lifecycle inspection.
func (r *Runtime) Server() *http.Server { return r.server }

// Run serves until the context is cancelled and then shuts down safely.
func (r *Runtime) Run(ctx context.Context) error {
	if r == nil || r.server == nil {
		return errors.New("runtime is not initialized")
	}
	baseContext, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	r.server.BaseContext = func(net.Listener) context.Context { return baseContext }
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- r.server.ListenAndServe()
	}()
	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := r.server.Shutdown(shutdownCtx); err != nil {
			_ = r.server.Close()
			<-serveResult
			return err
		}
		err := <-serveResult
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
