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

// Package oidc is the Shoplive in-house implementation of auth.OIDCService.
//
// It replaces the closed-source enterprise OIDC RP that ships in upstream's
// `e/` submodule. The implementation uses OIDC discovery
// (`<issuer>/.well-known/openid-configuration`) so any spec-compliant IdP works,
// not just Keycloak.
//
// File layout:
//
//	service.go    Service struct, OIDCService interface methods, auth-request
//	              construction (state, nonce, PKCE, IdP redirect URL).
//	discovery.go  Cached oidc.Provider lookup keyed by issuer URL.
//	callback.go   Code → token exchange, ID-token verification, user upsert,
//	              Web session / SSH+TLS cert issuance, audit event emission.
//	claims.go     ID-token claims → Teleport traits → roles mapping.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/defaults"
)

// Service implements auth.OIDCService.
type Service struct {
	auth      *auth.Server
	logger    *slog.Logger
	providers *providerCache // discovery cache; keyed by issuer URL.
}

// New returns a Service ready to register via authServer.SetOIDCService.
func New(authServer *auth.Server, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		auth:      authServer,
		logger:    logger,
		providers: newProviderCache(),
	}
}

// CreateOIDCAuthRequest persists an auth request and returns it with the IdP
// authorization URL set on RedirectURL.
func (s *Service) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	return s.createAuthRequest(ctx, req, defaults.OIDCAuthRequestTTL)
}

// CreateOIDCAuthRequestForMFA is the per-session MFA variant; identical to
// the standard login path for now.
func (s *Service) CreateOIDCAuthRequestForMFA(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	return s.createAuthRequest(ctx, req, defaults.OIDCAuthRequestTTL)
}

func (s *Service) createAuthRequest(ctx context.Context, req types.OIDCAuthRequest, ttl time.Duration) (*types.OIDCAuthRequest, error) {
	connector, err := s.auth.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err, "loading OIDC connector %q", req.ConnectorID)
	}

	// MFA flow: BeginSSOMFAChallenge sets req.CheckUser=true and supplies a
	// PkceVerifier. Apply the connector's MFA settings (separate client,
	// stricter prompt/acr/max_age) when configured.
	isMFA := req.CheckUser
	if isMFA {
		if mfaAware, ok := connector.(interface{ IsMFAEnabled() bool }); ok && mfaAware.IsMFAEnabled() {
			if err := connector.WithMFASettings(); err != nil {
				return nil, trace.Wrap(err, "applying MFA settings to OIDC connector %q", connector.GetName())
			}
		}
	}

	provider, err := s.providers.get(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err, "OIDC provider discovery for %q", connector.GetIssuerURL())
	}

	state, err := randomToken(32)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	nonce, err := randomToken(32)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.StateToken = state

	redirectURI := pickRedirectURL(connector)
	if redirectURI == "" {
		return nil, trace.BadParameter("OIDC connector %q has no redirect_url configured", connector.GetName())
	}

	authURL, err := url.Parse(provider.Endpoint().AuthURL)
	if err != nil {
		return nil, trace.Wrap(err, "parsing IdP authorization endpoint")
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", connector.GetClientID())
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", joinScopes(connector.GetScope()))
	q.Set("state", state)
	q.Set("nonce", nonce)

	// MFA flow forces a fresh authentication regardless of an existing IdP
	// session — that's the whole point of per-session MFA.
	prompt := connector.GetPrompt()
	if isMFA {
		prompt = "login"
	}
	if prompt != "" {
		q.Set("prompt", prompt)
	}
	if acr := connector.GetACR(); acr != "" {
		q.Set("acr_values", acr)
	}
	if maxAge, ok := connector.GetMaxAge(); ok {
		q.Set("max_age", strconv.Itoa(int(maxAge.Seconds())))
	} else if isMFA {
		// No explicit MaxAge but MFA flow → demand reauth-now via max_age=0.
		q.Set("max_age", "0")
	}

	// PKCE: in the MFA flow the verifier is supplied by the caller
	// (BeginSSOMFAChallenge); reuse it. In the login flow we generate one
	// here when the connector opts in.
	switch {
	case req.PkceVerifier != "":
		q.Set("code_challenge", pkceS256Challenge(req.PkceVerifier))
		q.Set("code_challenge_method", "S256")
	case pkceEnabled(connector):
		verifier, err := randomToken(64)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		req.PkceVerifier = verifier
		q.Set("code_challenge", pkceS256Challenge(verifier))
		q.Set("code_challenge_method", "S256")
	}

	authURL.RawQuery = q.Encode()
	req.RedirectURL = authURL.String()

	// Persist nonce alongside the request so we can verify it after callback.
	// OIDCAuthRequest has no nonce field; we stash it in the request type's
	// existing free-form Type field, prefixed so it doesn't collide.
	if req.Type != "" {
		// Caller-provided Type takes precedence; we still tuck the nonce in.
		req.Type = req.Type + "|nonce=" + nonce
	} else {
		req.Type = "nonce=" + nonce
	}

	if err := s.auth.Services.CreateOIDCAuthRequest(ctx, req, ttl); err != nil {
		return nil, trace.Wrap(err)
	}

	s.logger.InfoContext(ctx, "OIDC auth request created",
		"connector", req.ConnectorID,
		"state", state,
		"issuer", connector.GetIssuerURL(),
		"redirect_uri", redirectURI,
		"pkce", req.PkceVerifier != "",
		"mfa_flow", isMFA,
	)
	return &req, nil
}

func pickRedirectURL(c types.OIDCConnector) string {
	if urls := c.GetRedirectURLs(); len(urls) > 0 {
		return urls[0]
	}
	return ""
}

func joinScopes(extra []string) string {
	scopes := append([]string{"openid"}, extra...)
	// space-separated per OAuth 2.0 spec.
	return joinSpace(dedupe(scopes))
}

// dedupe returns ss with empty strings removed and duplicates collapsed,
// preserving first-seen order. Extracted so it can be unit-tested
// independently of joinScopes.
func dedupe(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func joinSpace(in []string) string {
	switch len(in) {
	case 0:
		return ""
	case 1:
		return in[0]
	}
	out := in[0]
	for _, s := range in[1:] {
		out += " " + s
	}
	return out
}

// pkceEnabled returns true when the connector opts into PKCE. Uses the
// connector's own IsPKCEEnabled() so we stay aligned with how the resource
// type itself interprets pkce_mode (the field is the typed
// constants.OIDCPKCEMode, not a plain string).
func pkceEnabled(c types.OIDCConnector) bool {
	type pkceChecker interface{ IsPKCEEnabled() bool }
	if v, ok := c.(pkceChecker); ok {
		return v.IsPKCEEnabled()
	}
	return false
}

func pkceS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// extractNonce pulls the nonce we tucked into req.Type back out.
func extractNonce(req *types.OIDCAuthRequest) string {
	const sep = "nonce="
	t := req.Type
	idx := indexOfSep(t, sep)
	if idx < 0 {
		return ""
	}
	t = t[idx+len(sep):]
	// nonce continues to end of string or until next | separator.
	for i, c := range t {
		if c == '|' {
			return t[:i]
		}
	}
	return t
}

func indexOfSep(s, sep string) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", trace.Wrap(err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// compile-time assertion that Service implements auth.OIDCService.
var _ auth.OIDCService = (*Service)(nil)

// keep authclient import in service.go even though only callback.go uses
// it directly — needed for the interface assertion above.
var _ = (*authclient.OIDCAuthResponse)(nil)
