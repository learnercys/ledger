// Package auth authenticates callers with OIDC access tokens.
//
// Every request needs a bearer token signed by the configured issuer, issued
// for this API (audience), and carrying a tenant_id claim. The tenant always
// comes from the token, never from the request body, so a caller can't act
// on another tenant's ledger by naming its accounts.
package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
)

// Scopes understood by the API.
const (
	ScopeRead  = "ledger.read"
	ScopeWrite = "ledger.write"
)

// Principal is the authenticated caller.
type Principal struct {
	Subject string
	Tenant  string
	Scopes  []string
}

// Has reports whether the caller was granted scope.
func (p Principal) Has(scope string) bool { return slices.Contains(p.Scopes, scope) }

type ctxKey struct{}

// FromContext returns the caller set by the interceptor.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// WithPrincipal stores a caller in ctx (used by tests).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// Verifier checks a token's signature, issuer and expiry.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*oidc.IDToken, error)
}

// NewRemoteVerifier verifies tokens against the issuer's published keys.
// jwksURL may differ from the issuer's public URL (for example inside a
// container network), but the token's iss claim must still equal issuer.
func NewRemoteVerifier(ctx context.Context, issuer, jwksURL string) Verifier {
	keys := oidc.NewRemoteKeySet(ctx, jwksURL)
	// Access tokens aren't ID tokens: the audience is our API, not an OIDC
	// client ID, so the client ID check is skipped and Authenticator checks aud.
	return oidc.NewVerifier(issuer, keys, &oidc.Config{SkipClientIDCheck: true})
}

// Authenticator turns bearer tokens into Principals and enforces scopes.
type Authenticator struct {
	Verifier Verifier
	Audience string
	// RequiredScope maps a procedure (e.g. "/ledger.v1.LedgerService/PostTransaction")
	// to the scope it needs. Procedures not listed are denied.
	RequiredScope map[string]string
}

type claims struct {
	Tenant string `json:"tenant_id"`
	Scope  string `json:"scope"`
}

var errMissingToken = errors.New("missing bearer token")

// Authenticate verifies the Authorization header value.
func (a *Authenticator) Authenticate(ctx context.Context, header string) (Principal, error) {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || raw == "" {
		return Principal{}, errMissingToken
	}
	tok, err := a.Verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, fmt.Errorf("invalid token: %w", err)
	}
	if !slices.Contains(tok.Audience, a.Audience) {
		return Principal{}, fmt.Errorf("token not issued for audience %q", a.Audience)
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("read claims: %w", err)
	}
	if c.Tenant == "" {
		return Principal{}, errors.New("token has no tenant_id claim")
	}
	return Principal{Subject: tok.Subject, Tenant: c.Tenant, Scopes: strings.Fields(c.Scope)}, nil
}

// Interceptor authenticates every unary call and checks its scope.
func (a *Authenticator) Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			p, err := a.Authenticate(ctx, req.Header().Get("Authorization"))
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			scope, ok := a.RequiredScope[req.Spec().Procedure]
			if !ok || !p.Has(scope) {
				return nil, connect.NewError(connect.CodePermissionDenied,
					fmt.Errorf("%s requires scope %q", req.Spec().Procedure, scope))
			}
			return next(WithPrincipal(ctx, p), req)
		}
	}
}
