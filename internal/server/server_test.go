package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/gen/ledger/v1/ledgerv1connect"
	"github.com/learnercys/ledger/internal/auth"
	"github.com/learnercys/ledger/internal/auth/authtest"
	"github.com/learnercys/ledger/internal/ledger"
)

// fakeStore records which tenant each call was made for.
type fakeStore struct {
	Store  // unimplemented methods panic, which no test should reach
	tenant string
	err    error
}

func (f *fakeStore) GetAccount(_ context.Context, tenant, id string) (*ledgerv1.Account, error) {
	f.tenant = tenant
	if f.err != nil {
		return nil, f.err
	}
	return &ledgerv1.Account{Id: id}, nil
}

func (f *fakeStore) PostTransaction(_ context.Context, tenant string, _ *ledgerv1.PostTransactionRequest) (*ledgerv1.Transaction, error) {
	f.tenant = tenant
	if f.err != nil {
		return nil, f.err
	}
	return &ledgerv1.Transaction{Id: "txn_1"}, nil
}

const issuerURL = "https://issuer.test/realms/ledger"

var full = authtest.Claims{Audience: []string{"ledger-api"}, Tenant: "acme", Scope: "ledger.read ledger.write"}

func with(c authtest.Claims, f func(*authtest.Claims)) authtest.Claims {
	f(&c)
	return c
}

// newClient serves the real handler and auth interceptor over HTTP and
// returns a client that sends token (if not empty).
func newClient(t *testing.T, store *fakeStore, iss *authtest.Issuer, token string) ledgerv1connect.LedgerServiceClient {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn := &auth.Authenticator{Verifier: iss.Verifier(), Audience: "ledger-api", RequiredScope: RequiredScopes}
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewLedgerServiceHandler(&Server{Store: store, Log: log},
		connect.WithInterceptors(authn.Interceptor())))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	addToken := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, req)
		}
	})
	return ledgerv1connect.NewLedgerServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(addToken))
}

func TestAuthentication(t *testing.T) {
	iss := authtest.NewIssuer(issuerURL)
	tests := []struct {
		name  string
		token string
		want  connect.Code
	}{
		{"valid", iss.Token(full), 0},
		{"no token", "", connect.CodeUnauthenticated},
		{"garbage", "not-a-jwt", connect.CodeUnauthenticated},
		{"expired", iss.Token(with(full, func(c *authtest.Claims) { c.Expiry = time.Now().Add(-time.Minute) })), connect.CodeUnauthenticated},
		{"wrong issuer", iss.Token(with(full, func(c *authtest.Claims) { c.Issuer = "https://evil.test" })), connect.CodeUnauthenticated},
		{"wrong audience", iss.Token(with(full, func(c *authtest.Claims) { c.Audience = []string{"other-api"} })), connect.CodeUnauthenticated},
		{"no tenant", iss.Token(with(full, func(c *authtest.Claims) { c.Tenant = "" })), connect.CodeUnauthenticated},
		{"signed by another key", authtest.NewIssuer(issuerURL).Token(full), connect.CodeUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, &fakeStore{}, iss, tt.token)
			_, err := client.GetAccount(context.Background(), &ledgerv1.GetAccountRequest{Id: "acct_1"})
			if got := connect.CodeOf(err); err != nil && got != tt.want || err == nil && tt.want != 0 {
				t.Fatalf("got %v, want code %v", err, tt.want)
			}
		})
	}
}

func TestScopes(t *testing.T) {
	iss := authtest.NewIssuer(issuerURL)
	readOnly := iss.Token(with(full, func(c *authtest.Claims) { c.Scope = "ledger.read" }))
	client := newClient(t, &fakeStore{}, iss, readOnly)

	if _, err := client.GetAccount(context.Background(), &ledgerv1.GetAccountRequest{Id: "a"}); err != nil {
		t.Fatalf("read with ledger.read: %v", err)
	}
	_, err := client.PostTransaction(context.Background(), &ledgerv1.PostTransactionRequest{})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("write with only ledger.read: got %v, want PermissionDenied", err)
	}
}

// The tenant is taken from the token. Nothing in the request can change it.
func TestTenantComesFromToken(t *testing.T) {
	iss := authtest.NewIssuer(issuerURL)
	store := &fakeStore{}
	client := newClient(t, store, iss, iss.Token(with(full, func(c *authtest.Claims) { c.Tenant = "globex" })))
	if _, err := client.GetAccount(context.Background(), &ledgerv1.GetAccountRequest{Id: "acct_of_acme"}); err != nil {
		t.Fatal(err)
	}
	if store.tenant != "globex" {
		t.Fatalf("store called for tenant %q, want globex", store.tenant)
	}
}

func TestErrorMapping(t *testing.T) {
	iss := authtest.NewIssuer(issuerURL)
	tests := []struct {
		err  error
		want connect.Code
	}{
		{ledger.ErrInvalid, connect.CodeInvalidArgument},
		{ledger.ErrNotFound, connect.CodeNotFound},
		{ledger.ErrInsufficientFunds, connect.CodeFailedPrecondition},
		{ledger.ErrAlreadyReversed, connect.CodeFailedPrecondition},
		{ledger.ErrIdempotencyMismatch, connect.CodeAlreadyExists},
		{errors.New("pq: connection refused at 10.0.0.5"), connect.CodeInternal},
	}
	for _, tt := range tests {
		client := newClient(t, &fakeStore{err: tt.err}, iss, iss.Token(full))
		_, err := client.PostTransaction(context.Background(), &ledgerv1.PostTransactionRequest{})
		if connect.CodeOf(err) != tt.want {
			t.Errorf("%v: got %v, want %v", tt.err, connect.CodeOf(err), tt.want)
		}
		if tt.want == connect.CodeInternal {
			var ce *connect.Error
			if errors.As(err, &ce) && ce.Message() != "internal error" {
				t.Errorf("internal details leaked to the caller: %q", ce.Message())
			}
		}
	}
}

// Each RPC produces a server span that records which tenant made the call.
func TestRPCSpanCarriesTenant(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otelInterceptor, err := otelconnect.NewInterceptor(otelconnect.WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	iss := authtest.NewIssuer(issuerURL)
	authn := &auth.Authenticator{Verifier: iss.Verifier(), Audience: "ledger-api", RequiredScope: RequiredScopes}
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewLedgerServiceHandler(
		&Server{Store: &fakeStore{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		connect.WithInterceptors(otelInterceptor, authn.Interceptor())))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	token := iss.Token(with(full, func(c *authtest.Claims) { c.Tenant = "globex" }))
	addToken := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
	client := ledgerv1connect.NewLedgerServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(addToken))
	if _, err := client.GetAccount(context.Background(), &ledgerv1.GetAccountRequest{Id: "acct_1"}); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "ledger.v1.LedgerService/GetAccount" {
		t.Fatalf("unexpected spans: %v", spans)
	}
	var tenant string
	for _, a := range spans[0].Attributes() {
		if a.Key == "tenant.id" {
			tenant = a.Value.AsString()
		}
	}
	if tenant != "globex" {
		t.Fatalf("span tenant.id = %q, want globex", tenant)
	}
}
