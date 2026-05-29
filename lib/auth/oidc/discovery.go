/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package oidc

import (
	"context"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gravitational/trace"
)

// providerEntry caches a discovered OIDC provider for a given issuer URL.
// Discovery is HTTP-bound, so we hold one entry per issuer for the process
// lifetime — JWKs inside the provider auto-refresh via go-oidc internals
// (it re-fetches when an unknown kid is seen).
type providerEntry struct {
	provider *oidc.Provider
	fetched  time.Time
}

type providerCache struct {
	mu sync.Mutex
	m  map[string]*providerEntry
}

func newProviderCache() *providerCache {
	return &providerCache{m: make(map[string]*providerEntry)}
}

// get returns a provider for the given issuer URL, performing OIDC discovery
// once per issuer and caching the result.
func (c *providerCache) get(ctx context.Context, issuer string) (*oidc.Provider, error) {
	c.mu.Lock()
	if e, ok := c.m[issuer]; ok {
		c.mu.Unlock()
		return e.provider, nil
	}
	c.mu.Unlock()

	// Cold path — release lock during HTTP, then double-check.
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, trace.Wrap(err, "OIDC discovery failed for %q", issuer)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[issuer]; ok {
		// Lost the race — return the value the other goroutine cached.
		return e.provider, nil
	}
	c.m[issuer] = &providerEntry{provider: provider, fetched: time.Now()}
	return provider, nil
}

// invalidate drops the cached entry for an issuer (used when the connector
// configuration changes and we want subsequent calls to re-discover).
func (c *providerCache) invalidate(issuer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, issuer)
}
