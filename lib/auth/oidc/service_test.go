/*
 * Teleport — Shoplive fork
 */

package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
)

func TestRandomToken(t *testing.T) {
	tok, err := randomToken(32)
	require.NoError(t, err)
	// base64url of 32 raw bytes is 43 chars (no padding).
	require.Len(t, tok, 43)

	// Different invocations produce different tokens (CSPRNG smoke test).
	tok2, err := randomToken(32)
	require.NoError(t, err)
	require.NotEqual(t, tok, tok2)
}

func TestPKCES256Challenge(t *testing.T) {
	verifier := "shoplive-test-verifier-with-enough-entropy-12345"
	got := pkceS256Challenge(verifier)

	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	require.Equal(t, want, got)

	// And it should match the published example from RFC 7636 §4.6.
	rfcVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	rfcChallenge := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	require.Equal(t, rfcChallenge, pkceS256Challenge(rfcVerifier),
		"PKCE S256 challenge must match RFC 7636 example")
}

func TestJoinScopes(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "openid is added unconditionally",
			in:   nil,
			want: "openid",
		},
		{
			name: "openid is not duplicated",
			in:   []string{"openid", "email"},
			want: "openid email",
		},
		{
			name: "extras are appended in order",
			in:   []string{"email", "profile", "groups"},
			want: "openid email profile groups",
		},
		{
			name: "empty entries are dropped",
			in:   []string{"", "email", "", "profile"},
			want: "openid email profile",
		},
		{
			name: "duplicates are removed",
			in:   []string{"email", "email", "profile"},
			want: "openid email profile",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, joinScopes(tc.in))
		})
	}
}

func TestExtractNonce(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want string
	}{
		{name: "nonce only", typ: "nonce=abc123", want: "abc123"},
		{name: "no nonce", typ: "", want: ""},
		{name: "no nonce, type set", typ: "saml-test", want: ""},
		{name: "nonce after a |", typ: "saml-test|nonce=abc123", want: "abc123"},
		{name: "nonce with trailing |", typ: "nonce=abc123|other=foo", want: "abc123"},
		{name: "stray sep before nonce", typ: "x=y|nonce=zzz", want: "zzz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &types.OIDCAuthRequest{Type: tc.typ}
			require.Equal(t, tc.want, extractNonce(req))
		})
	}
}

func TestPickRedirectURL(t *testing.T) {
	require.Equal(t, "", pickRedirectURL(fakeConnector{redirectURLs: nil}),
		"no redirect URLs → empty string")
	require.Equal(t, "https://a", pickRedirectURL(fakeConnector{redirectURLs: []string{"https://a"}}))
	require.Equal(t, "https://a", pickRedirectURL(fakeConnector{redirectURLs: []string{"https://a", "https://b"}}),
		"first redirect URL wins")
}

func TestPKCEEnabled(t *testing.T) {
	require.False(t, pkceEnabled(fakeConnector{}),
		"connector without IsPKCEEnabled defaults to disabled")
	require.True(t, pkceEnabled(fakeConnector{pkce: true}))
	require.False(t, pkceEnabled(fakeConnector{pkce: false}))
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"a", "b", "a", "", "c", "b"})
	want := []string{"a", "b", "c"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dedupe mismatch (-want +got):\n%s", diff)
	}
	require.Empty(t, dedupe(nil))
	require.Empty(t, dedupe([]string{"", "", ""}))
}

// fakeConnector is a minimal stub satisfying just the methods our service.go
// helpers actually call. We avoid pulling in the full OIDCConnectorV3 setup
// so unit tests stay decoupled from proto plumbing.
type fakeConnector struct {
	types.OIDCConnector // embed nil to absorb the rest of the interface

	name         string
	issuerURL    string
	clientID     string
	clientSecret string
	redirectURLs []string
	scopes       []string
	prompt       string
	acr          string
	pkce         bool
}

func (f fakeConnector) GetName() string         { return f.name }
func (f fakeConnector) GetIssuerURL() string    { return f.issuerURL }
func (f fakeConnector) GetClientID() string     { return f.clientID }
func (f fakeConnector) GetClientSecret() string { return f.clientSecret }
func (f fakeConnector) GetRedirectURLs() []string {
	return append([]string(nil), f.redirectURLs...)
}
func (f fakeConnector) GetScope() []string { return append([]string(nil), f.scopes...) }
func (f fakeConnector) GetPrompt() string  { return f.prompt }
func (f fakeConnector) GetACR() string     { return f.acr }
func (f fakeConnector) IsPKCEEnabled() bool {
	return f.pkce
}

// Sanity check: extractNonce + a realistic Type round-trips.
func TestNonceRoundTrip(t *testing.T) {
	for _, n := range []string{
		"abc",
		strings.Repeat("a", 64),
		"with-dashes-and_underscores",
	} {
		req := &types.OIDCAuthRequest{Type: "nonce=" + n}
		assert.Equal(t, n, extractNonce(req))
	}
}
