// Package authtest issues signed tokens for tests, without a real OIDC server.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer is a fake token issuer with its own RSA key.
type Issuer struct {
	URL string
	key *rsa.PrivateKey
}

// NewIssuer creates an issuer with a fresh key.
func NewIssuer(url string) *Issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return &Issuer{URL: url, key: key}
}

// Verifier returns a verifier that trusts this issuer's key.
func (i *Issuer) Verifier() *oidc.IDTokenVerifier {
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{i.key.Public()}}
	return oidc.NewVerifier(i.URL, keys, &oidc.Config{SkipClientIDCheck: true})
}

// Claims describes a token to issue. Zero values get sensible defaults.
type Claims struct {
	Issuer   string
	Subject  string
	Audience []string
	Tenant   string
	Scope    string
	Expiry   time.Time
}

// Token signs a token with the given claims.
func (i *Issuer) Token(c Claims) string {
	if c.Issuer == "" {
		c.Issuer = i.URL
	}
	if c.Subject == "" {
		c.Subject = "service-account"
	}
	if c.Expiry.IsZero() {
		c.Expiry = time.Now().Add(time.Hour)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.key}, nil)
	if err != nil {
		panic(err)
	}
	raw, err := jwt.Signed(signer).
		Claims(jwt.Claims{
			Issuer:   c.Issuer,
			Subject:  c.Subject,
			Audience: c.Audience,
			Expiry:   jwt.NewNumericDate(c.Expiry),
			IssuedAt: jwt.NewNumericDate(time.Now()),
		}).
		Claims(map[string]any{"tenant_id": c.Tenant, "scope": c.Scope}).
		Serialize()
	if err != nil {
		panic(err)
	}
	return raw
}
