// Command ledger serves the ledger API over gRPC and HTTP/JSON.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/otelconnect"

	"github.com/learnercys/ledger/gen/ledger/v1/ledgerv1connect"
	"github.com/learnercys/ledger/internal/auth"
	"github.com/learnercys/ledger/internal/server"
	"github.com/learnercys/ledger/internal/store"
	"github.com/learnercys/ledger/internal/telemetry"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "ledger")
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, "ledger")
	if err != nil {
		return err
	}
	defer func() { _ = shutdown(context.Background()) }()

	st, err := store.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer st.Close()

	issuer := mustEnv("OIDC_ISSUER")
	jwks := os.Getenv("OIDC_JWKS_URL")
	if jwks == "" {
		jwks = issuer + "/protocol/openid-connect/certs"
	}
	authn := &auth.Authenticator{
		Verifier:      auth.NewRemoteVerifier(ctx, issuer, jwks),
		Audience:      mustEnv("OIDC_AUDIENCE"),
		RequiredScope: server.RequiredScopes,
	}

	// RPC spans and metrics (rpc.server.duration, …) for every call, including
	// rejected ones.
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewLedgerServiceHandler(
		&server.Server{Store: st, Log: log},
		// Tracing and logging run outermost so they also cover rejected calls.
		connect.WithInterceptors(otelInterceptor, server.LoggingInterceptor(log), authn.Interceptor()),
	))
	// Reflection lets grpcurl and Postman discover the API. It exposes the
	// schema only; every RPC still requires a token.
	reflector := grpcreflect.NewStaticReflector(ledgerv1connect.LedgerServiceName)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              envOr("ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		Protocols:         new(http.Protocols),
	}
	// gRPC needs HTTP/2; serve it without TLS (h2c) behind a TLS-terminating proxy.
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", srv.Addr, "issuer", issuer)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required environment variable", "key", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
