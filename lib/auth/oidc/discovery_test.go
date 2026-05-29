/*
 * Teleport — Shoplive fork
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
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

// TestProviderCache_ConcurrentGet stresses the cold-path-then-double-check
// pattern inside providerCache.get. N goroutines pile onto the same issuer;
// after the dust settles all of them must see the same *Provider, and the
// IdP's discovery endpoint should have been hit at most N times (in practice
// usually 1 — but we don't assert the exact count because the cache releases
// its lock during the HTTP call, so a small race window between goroutines
// is permitted by the current implementation).
func TestProviderCache_ConcurrentGet(t *testing.T) {
	idp := newFakeIdP(t)
	cache := newProviderCache()

	const N = 10

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		got    = make([]*oidc.Provider, 0, N)
		errors = make([]error, 0)
	)
	wg.Add(N)
	start := make(chan struct{})
	for range N {
		go func() {
			defer wg.Done()
			<-start
			p, err := cache.get(ctx, idp.srv.URL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errors = append(errors, err)
				return
			}
			got = append(got, p)
		}()
	}
	close(start)
	wg.Wait()

	require.Empty(t, errors, "no goroutine should see an error")
	require.Len(t, got, N)

	first := got[0]
	require.NotNil(t, first)
	for i, p := range got[1:] {
		require.Same(t, first, p,
			"goroutine %d returned a different *Provider — cache lost an entry", i+1)
	}

	hits := idp.discoveryHits.Load()
	require.GreaterOrEqual(t, hits, int64(1),
		"at least one discovery hit is required to populate the cache")
	require.LessOrEqual(t, hits, int64(N),
		"discovery hits must not exceed the number of callers")
}

// TestProviderCache_IssuerMismatch verifies we don't paper over an IdP that
// advertises a different `issuer` than the URL we discovered it at —
// go-oidc's NewProvider is strict about this and our cache must surface the
// mismatch as an error without poisoning the entry.
func TestProviderCache_IssuerMismatch(t *testing.T) {
	idp := newFakeIdP(t)
	// Discovery URL is idp.srv.URL but the doc advertises a different issuer.
	idp.discoveryResponse["issuer"] = "https://impostor.example/realms/main"

	cache := newProviderCache()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := cache.get(ctx, idp.srv.URL)
	require.Error(t, err, "issuer mismatch must surface as an error")

	cache.mu.Lock()
	_, cached := cache.m[idp.srv.URL]
	cache.mu.Unlock()
	require.False(t, cached, "failed discovery must not be cached")
}
