/*
 * Teleport — Shoplive fork
 */

package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeIdP is the minimum HTTP surface go-oidc's NewProvider talks to during
// discovery: a `/.well-known/openid-configuration` endpoint and a JWKS URL.
// Counts hits so we can assert the providerCache short-circuits subsequent
// requests for the same issuer.
type fakeIdP struct {
	srv               *httptest.Server
	discoveryHits     atomic.Int64
	jwksHits          atomic.Int64
	discoveryResponse map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()

	idp := &fakeIdP{}
	mux := http.NewServeMux()
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)

	idp.discoveryResponse = map[string]any{
		"issuer":                 idp.srv.URL,
		"authorization_endpoint": idp.srv.URL + "/auth",
		"token_endpoint":         idp.srv.URL + "/token",
		"jwks_uri":               idp.srv.URL + "/jwks",
		"userinfo_endpoint":      idp.srv.URL + "/userinfo",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
	}

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.discoveryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.discoveryResponse)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		idp.jwksHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	})

	return idp
}

func TestProviderCache_CachesByIssuer(t *testing.T) {
	idp := newFakeIdP(t)
	cache := newProviderCache()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// First call hits discovery once.
	p1, err := cache.get(ctx, idp.srv.URL)
	require.NoError(t, err)
	require.NotNil(t, p1)
	require.EqualValues(t, 1, idp.discoveryHits.Load())

	// Second call returns the same provider — no extra discovery hit.
	p2, err := cache.get(ctx, idp.srv.URL)
	require.NoError(t, err)
	require.Same(t, p1, p2, "cache should return the same *Provider for the same issuer")
	require.EqualValues(t, 1, idp.discoveryHits.Load(), "second get must reuse cached entry")
}

func TestProviderCache_Invalidate(t *testing.T) {
	idp := newFakeIdP(t)
	cache := newProviderCache()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := cache.get(ctx, idp.srv.URL)
	require.NoError(t, err)
	require.EqualValues(t, 1, idp.discoveryHits.Load())

	cache.invalidate(idp.srv.URL)

	_, err = cache.get(ctx, idp.srv.URL)
	require.NoError(t, err)
	require.EqualValues(t, 2, idp.discoveryHits.Load(),
		"after invalidate, the next get must re-discover")
}

func TestProviderCache_DifferentIssuers(t *testing.T) {
	a := newFakeIdP(t)
	b := newFakeIdP(t)
	cache := newProviderCache()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	pa, err := cache.get(ctx, a.srv.URL)
	require.NoError(t, err)
	pb, err := cache.get(ctx, b.srv.URL)
	require.NoError(t, err)

	require.NotSame(t, pa, pb, "different issuers must yield different providers")
	require.EqualValues(t, 1, a.discoveryHits.Load())
	require.EqualValues(t, 1, b.discoveryHits.Load())
}

func TestProviderCache_DiscoveryError(t *testing.T) {
	cache := newProviderCache()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// Bogus URL — discovery fetch should fail; cache must NOT memoize the
	// failure (so a transient outage doesn't poison the process).
	_, err := cache.get(ctx, "http://127.0.0.1:1/realms/nope")
	require.Error(t, err)

	cache.mu.Lock()
	_, cached := cache.m["http://127.0.0.1:1/realms/nope"]
	cache.mu.Unlock()
	require.False(t, cached, "failed discovery must not be cached")
}
