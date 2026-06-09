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
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gravitational/trace"
	"golang.org/x/oauth2"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/utils/keys/hardwarekey"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/loginrule"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
)

// ValidateOIDCAuthCallback finishes the OIDC SSO flow. It exchanges the auth
// code for tokens, verifies the ID-token signature + claims, maps claims to
// roles via the connector's claims_to_roles, upserts the Teleport user, and
// returns a Web session and/or SSH+TLS certs depending on what the original
// auth request asked for.
func (s *Service) ValidateOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error) {
	resp, isMFA, err := s.validateCallback(ctx, q)
	// MFA round-trips (both success and failure) are audited by the mfa
	// service via its own chain that started at BeginSSOMFAChallenge —
	// emitting a UserSSOLoginFailureCode/LoginMethodOIDC here would split
	// MFA failures away from their MFA event stream and make them invisible
	// to SOC dashboards keyed on mfa events. Regular login (and any
	// pre-state-lookup failure where we can't yet tell which flow it is)
	// still emits.
	if !isMFA {
		s.emitLoginEvent(ctx, resp, err, q)
	}
	return resp, trace.Wrap(err)
}

// validateCallback runs the full callback validation. The second return
// (isMFA) is true once we've confirmed the original auth request was an
// MFA flow (req.CheckUser=true) — used by ValidateOIDCAuthCallback to
// route the audit event to either the regular login chain or the mfa
// service's chain. Failures before the auth-request lookup (bad query
// params, expired/replayed state) cannot know which flow they belong to
// and report isMFA=false so they're still audited as login events.
func (s *Service) validateCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, bool, error) {
	state, code, err := parseCallbackParams(q)
	if err != nil {
		return nil, false, trace.Wrap(err)
	}

	// Recover the original auth request (also gives us the persisted PKCE
	// verifier + nonce). Storage TTLs the request, so an expired/replayed
	// state surfaces here.
	req, err := s.auth.Services.GetOIDCAuthRequest(ctx, state)
	if err != nil {
		return nil, false, trace.Wrap(err, "looking up OIDC auth request for state")
	}
	// From here on we know which flow we're in; propagate to the caller so
	// MFA-flow failures route to the mfa service's audit chain instead of
	// the regular login event chain.
	isMFA := req.CheckUser

	connector, err := s.auth.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, isMFA, trace.Wrap(err, "loading OIDC connector %q", req.ConnectorID)
	}

	provider, err := s.providers.get(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, isMFA, trace.Wrap(err)
	}

	// Token exchange.
	oauth2Config := oauth2.Config{
		ClientID:     connector.GetClientID(),
		ClientSecret: connector.GetClientSecret(),
		RedirectURL:  pickRedirectURL(connector),
		Endpoint:     provider.Endpoint(),
	}
	exchangeOpts := []oauth2.AuthCodeOption{}
	if req.PkceVerifier != "" {
		exchangeOpts = append(exchangeOpts, oauth2.VerifierOption(req.PkceVerifier))
	}
	token, err := oauth2Config.Exchange(ctx, code, exchangeOpts...)
	if err != nil {
		return nil, isMFA, trace.AccessDenied("OIDC token exchange failed: %v", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, isMFA, trace.AccessDenied("OIDC token response missing id_token")
	}

	// ID-token verification: signature against JWKS, iss/aud/exp/iat.
	verifier := provider.Verifier(&oidc.Config{ClientID: connector.GetClientID()})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, isMFA, trace.AccessDenied("OIDC id_token verification failed: %v", err)
	}

	// Nonce check.
	expectedNonce := extractNonce(req)
	if expectedNonce != "" && idToken.Nonce != expectedNonce {
		return nil, isMFA, trace.AccessDenied("OIDC nonce mismatch")
	}

	// Pull claims from the id_token as a free-form map, then always fetch
	// /userinfo to augment them. Some IdPs (notably Google) split claims
	// between the id_token and the userinfo endpoint, so we can't rely on the
	// id_token alone. Merge semantics: id_token claims are authoritative —
	// mergeUserInfo only adds keys from /userinfo that are not already
	// present in claims, it never overwrites existing values. A failing
	// /userinfo is demoted to a warning so a flaky endpoint doesn't block
	// login when the id_token already carries enough claims.
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, isMFA, trace.Wrap(err, "decoding id_token claims")
	}
	if claims == nil {
		claims = map[string]any{}
	}
	if err := mergeUserInfo(ctx, provider, token, &claims); err != nil {
		s.logger.WarnContext(ctx, "Failed to fetch userinfo (continuing with id_token claims only)", "error", err)
	}

	// Translate claims → traits → role list.
	traits := claimsToTraits(claims)

	// Apply Login Rules so admins can rewrite/augment traits before they
	// reach claims_to_roles. Mirrors lib/auth/github.go.
	evalOut, err := s.auth.GetLoginRuleEvaluator().Evaluate(ctx, &loginrule.EvaluationInput{
		Traits: traits,
	})
	if err != nil {
		return nil, isMFA, trace.Wrap(err, "evaluating login rules")
	}
	traits = evalOut.Traits
	if len(evalOut.AppliedRules) > 0 {
		s.logger.InfoContext(ctx, "OIDC login rules applied",
			"connector", connector.GetName(),
			"applied_rules", evalOut.AppliedRules,
		)
	}

	username := usernameFromClaims(connector, traits)
	if username == "" {
		return nil, isMFA, trace.AccessDenied("OIDC claims did not yield a username (none of preferred_username/email/sub were present)")
	}

	// Per-session MFA flow short-circuits here. BeginSSOMFAChallenge marked
	// `req.CheckUser=true` and persisted an MFA session keyed by the state
	// token; we just need to verify the IdP-authenticated identity matches
	// and mint an MFAToken. No user upsert, no session/cert issuance.
	if isMFA {
		resp, err := s.handleMFACallback(ctx, req, username, connector.GetName())
		return resp, true, trace.Wrap(err)
	}

	roleWarnings, roles := services.TraitsToRoles(connector.GetTraitMappings(), traits)
	if len(roles) == 0 {
		s.logger.WarnContext(ctx, "OIDC claims mapped to no roles",
			"connector", connector.GetName(),
			"username", username,
			"warnings", roleWarnings,
		)
		return nil, isMFA, trace.AccessDenied("OIDC user %q has no matching roles via claims_to_roles", username)
	}

	// Resolve session TTL bounded by the role's max TTL and the requested cert TTL.
	resolved, err := services.FetchRolesWithContext(roles, s.auth, services.RoleTemplateContext{
		Username: username,
		Traits:   traits,
	})
	if err != nil {
		return nil, isMFA, trace.Wrap(err)
	}
	roleTTL := resolved.AdjustSessionTTL(apidefaults.MaxCertDuration)
	sessionTTL := utils.MinTTL(roleTTL, req.CertTTL)
	if sessionTTL <= 0 {
		sessionTTL = roleTTL
	}

	// Upsert the user — in test flow we skip mutating the backend.
	user, err := s.upsertUser(ctx, connector.GetName(), username, roles, traits, sessionTTL, req.SSOTestFlow)
	if err != nil {
		return nil, isMFA, trace.Wrap(err)
	}

	// Login hooks (e.g. usage reporting, login state). Mirror the GitHub flow.
	if !req.SSOTestFlow {
		if err := s.auth.CallLoginHooks(ctx, user); err != nil {
			s.logger.WarnContext(ctx, "OIDC login hooks returned an error", "error", err)
		}
	}

	userState, err := s.auth.GetUserOrLoginState(ctx, user.GetName())
	if err != nil {
		return nil, isMFA, trace.Wrap(err)
	}

	identity := types.ExternalIdentity{
		ConnectorID: connector.GetName(),
		Username:    username,
	}

	if req.SSOTestFlow {
		return &authclient.OIDCAuthResponse{
			Req:      authclient.OIDCAuthRequest{ConnectorID: req.ConnectorID, CSRFToken: req.CSRFToken},
			Identity: identity,
			Username: username,
		}, isMFA, nil
	}

	resp, err := s.makeAuthResponse(ctx, req, userState, identity, sessionTTL)
	return resp, isMFA, trace.Wrap(err)
}

// makeAuthResponse mirrors auth.makeGithubAuthResponse — produces a Web
// session, SSH+TLS certs, or both depending on what the original request asked
// for.
func (s *Service) makeAuthResponse(
	ctx context.Context,
	req *types.OIDCAuthRequest,
	userState services.UserState,
	identity types.ExternalIdentity,
	sessionTTL time.Duration,
) (*authclient.OIDCAuthResponse, error) {
	resp := &authclient.OIDCAuthResponse{
		Req: authclient.OIDCAuthRequest{
			ConnectorID:       req.ConnectorID,
			CSRFToken:         req.CSRFToken,
			PublicKey:         nil,
			SSHPubKey:         req.SshPublicKey,
			TLSPubKey:         req.TlsPublicKey,
			CreateWebSession:  req.CreateWebSession,
			ClientRedirectURL: req.ClientRedirectURL,
		},
		Identity: identity,
		Username: userState.GetName(),
	}

	if req.CreateWebSession {
		session, err := s.auth.CreateWebSessionFromReq(ctx, auth.NewWebSessionRequest{
			User:                 userState.GetName(),
			Roles:                userState.GetRoles(),
			Traits:               userState.GetTraits(),
			SessionTTL:           sessionTTL,
			LoginTime:            s.auth.GetClock().Now().UTC(),
			LoginIP:              req.ClientLoginIP,
			LoginUserAgent:       req.ClientUserAgent,
			AttestWebSession:     true,
			CreateDeviceWebToken: true,
			Scope:                req.Scope,
		})
		if err != nil {
			return nil, trace.Wrap(err, "creating Web session")
		}
		resp.Session = session
	}

	if len(req.SshPublicKey) != 0 || len(req.TlsPublicKey) != 0 {
		sshCert, tlsCert, err := s.auth.CreateSessionCerts(ctx, &auth.SessionCertsRequest{
			UserState:               userState,
			SessionTTL:              sessionTTL,
			SSHPubKey:               req.SshPublicKey,
			TLSPubKey:               req.TlsPublicKey,
			SSHAttestationStatement: hardwarekey.AttestationStatementFromProto(req.SshAttestationStatement),
			TLSAttestationStatement: hardwarekey.AttestationStatementFromProto(req.TlsAttestationStatement),
			Compatibility:           req.Compatibility,
			RouteToCluster:          req.RouteToCluster,
			KubernetesCluster:       req.KubernetesCluster,
			LoginIP:                 req.ClientLoginIP,
			Scope:                   req.Scope,
		})
		if err != nil {
			return nil, trace.Wrap(err, "issuing session certs")
		}

		clusterName, err := s.auth.GetClusterName(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		hostCA, err := s.auth.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, false)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		resp.Cert = sshCert
		resp.TLSCert = tlsCert
		resp.HostSigners = append(resp.HostSigners, hostCA)
	}

	if opts, err := s.auth.ClientOptionsForLogin(userState); err == nil {
		resp.ClientOptions = opts
	}

	return resp, nil
}

// handleMFACallback completes the SSO-driven per-session MFA flow.
// The OIDC dance was just performed for an already-known Teleport user, so
// we don't upsert anything new — we just confirm the IdP-asserted identity
// matches the user who initiated the MFA challenge, then mint an MFA token.
func (s *Service) handleMFACallback(
	ctx context.Context,
	req *types.OIDCAuthRequest,
	idpUsername string,
	connectorName string,
) (*authclient.OIDCAuthResponse, error) {
	sd, err := s.auth.GetMFASession(ctx, req.StateToken)
	if err != nil {
		return nil, trace.Wrap(err, "looking up MFA session for state %q", req.StateToken)
	}
	if err := verifyMFAUsernameMatch(sd.Username, idpUsername); err != nil {
		return nil, trace.Wrap(err)
	}

	mfaToken, err := s.auth.UpsertMFASessionWithToken(ctx, sd)
	if err != nil {
		return nil, trace.Wrap(err, "issuing MFA token")
	}

	return &authclient.OIDCAuthResponse{
		Req: authclient.OIDCAuthRequest{
			ConnectorID: req.ConnectorID,
			CSRFToken:   req.CSRFToken,
		},
		Identity: types.ExternalIdentity{
			ConnectorID: connectorName,
			Username:    idpUsername,
		},
		Username: idpUsername,
		MFAToken: mfaToken,
	}, nil
}

func (s *Service) upsertUser(
	ctx context.Context,
	connectorName, username string,
	roles []string,
	traits map[string][]string,
	sessionTTL time.Duration,
	dryRun bool,
) (types.User, error) {
	expires := s.auth.GetClock().Now().UTC().Add(sessionTTL)

	user := &types.UserV2{
		Kind:    types.KindUser,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      username,
			Namespace: apidefaults.Namespace,
			Expires:   &expires,
		},
		Spec: types.UserSpecV2{
			Roles:  roles,
			Traits: traits,
			OIDCIdentities: []types.ExternalIdentity{{
				ConnectorID: connectorName,
				Username:    username,
			}},
			CreatedBy: types.CreatedBy{
				User: types.UserRef{Name: teleport.UserSystem},
				Time: s.auth.GetClock().Now().UTC(),
				Connector: &types.ConnectorRef{
					Type:     constants.OIDC,
					ID:       connectorName,
					Identity: username,
				},
			},
		},
	}

	if dryRun {
		return user, nil
	}

	existing, err := s.auth.Services.GetUser(ctx, username, false)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if existing != nil {
		ref := user.GetCreatedBy().Connector
		if !ref.IsSameProvider(existing.GetCreatedBy().Connector) {
			return nil, trace.AlreadyExists("user %q already exists and was not created by this OIDC connector", username)
		}
		user.SetRevision(existing.GetRevision())
		updated, err := s.auth.UpdateUser(ctx, user)
		return updated, trace.Wrap(err)
	}

	created, err := s.auth.CreateUser(ctx, user)
	return created, trace.Wrap(err)
}

func (s *Service) emitLoginEvent(ctx context.Context, resp *authclient.OIDCAuthResponse, loginErr error, q url.Values) {
	event := &apievents.UserLogin{
		Metadata: apievents.Metadata{
			Type: events.UserLoginEvent,
		},
		Method:             events.LoginMethodOIDC,
		ConnectionMetadata: authz.ConnectionMetadata(ctx),
	}
	if loginErr != nil {
		event.Code = events.UserSSOLoginFailureCode
		event.Status.Success = false
		event.Status.Error = trace.Unwrap(loginErr).Error()
		event.Status.UserMessage = loginErr.Error()
	} else {
		event.Code = events.UserSSOLoginCode
		event.Status.Success = true
		if resp != nil {
			event.User = resp.Username
		}
	}
	if err := s.auth.EmitAuditEvent(ctx, event); err != nil {
		s.logger.WarnContext(ctx, "Failed to emit OIDC login audit event", "error", err)
	}
}

// parseCallbackParams extracts and validates the OAuth2 callback query
// params. Returns trace.AccessDenied when the IdP signaled an error via
// the "error" / "error_description" pair, trace.BadParameter when "state"
// or "code" are missing. The split out of validateCallback exists so the
// boundary check has a dedicated unit test without needing an auth.Server.
func parseCallbackParams(q url.Values) (state, code string, err error) {
	if errParam := q.Get("error"); errParam != "" {
		return "", "", trace.AccessDenied(
			"OIDC IdP returned error %q: %s", errParam, q.Get("error_description"),
		)
	}
	state = q.Get("state")
	code = q.Get("code")
	switch {
	case state == "":
		return "", "", trace.BadParameter("missing 'state' query parameter")
	case code == "":
		return "", "", trace.BadParameter("missing 'code' query parameter")
	}
	return state, code, nil
}

// verifyMFAUsernameMatch confirms the IdP-asserted username matches the
// Teleport user who initiated the per-session MFA challenge. Extracted from
// handleMFACallback so the comparison has a unit test without needing the
// full MFA session storage stack.
func verifyMFAUsernameMatch(expectedFromSession, idpUsername string) error {
	if expectedFromSession != idpUsername {
		return trace.AccessDenied(
			"OIDC MFA user mismatch: session expects %q, IdP returned %q",
			expectedFromSession, idpUsername,
		)
	}
	return nil
}

// mergeUserInfo augments id_token claims with /userinfo fields. Some IdPs
// (Google in particular) split claims between the ID token and userinfo.
// Existing id_token keys are preserved as authoritative — only keys not
// already present in claims are copied from userinfo.
func mergeUserInfo(ctx context.Context, p *oidc.Provider, tok *oauth2.Token, claims *map[string]any) error {
	ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return trace.Wrap(err)
	}
	var extra map[string]any
	if err := ui.Claims(&extra); err != nil {
		return trace.Wrap(err)
	}
	for k, v := range extra {
		if _, exists := (*claims)[k]; exists {
			continue
		}
		(*claims)[k] = v
	}
	return nil
}
