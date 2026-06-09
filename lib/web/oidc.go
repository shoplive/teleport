/*
 * Teleport — Shoplive fork
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package web

import (
	"net"
	"net/http"
	"strings"

	"github.com/gravitational/trace"
	"github.com/julienschmidt/httprouter"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/client/sso"
	"github.com/gravitational/teleport/lib/httplib"
)

// oidcLoginWeb starts a browser-driven OIDC login. The Web UI redirects users
// here when they click an OIDC connector button on /web/login. The proxy asks
// the auth server to create an auth request (which delegates to the in-house
// OIDC service in lib/auth/oidc) and 302s the browser to the IdP URL.
//
// Mirrors githubLoginWeb.
func (h *Handler) oidcLoginWeb(w http.ResponseWriter, r *http.Request, p httprouter.Params) string {
	logger := h.logger.With("auth", "oidc")
	logger.DebugContext(r.Context(), "Web login start")

	req, err := ParseSSORequestParams(r)
	if err != nil {
		logger.ErrorContext(r.Context(), "Failed to extract SSO parameters from request", "error", err)
		return sso.LoginFailedRedirectURL
	}

	remoteAddr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		logger.ErrorContext(r.Context(), "Failed to parse request remote address", "error", err)
		return sso.LoginFailedRedirectURL
	}

	response, err := h.cfg.ProxyClient.CreateOIDCAuthRequest(r.Context(), types.OIDCAuthRequest{
		CSRFToken:         req.CSRFToken,
		ConnectorID:       req.ConnectorID,
		CreateWebSession:  true,
		ClientRedirectURL: req.ClientRedirectURL,
		ClientLoginIP:     remoteAddr,
		ClientUserAgent:   r.UserAgent(),
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "Error creating OIDC auth request", "error", err)
		return sso.LoginFailedRedirectURL
	}

	return response.RedirectURL
}

// oidcLoginConsole handles tsh CLI logins (tsh login --auth=<connector>).
//
// Mirrors githubLoginConsole.
func (h *Handler) oidcLoginConsole(w http.ResponseWriter, r *http.Request, p httprouter.Params) (any, error) {
	logger := h.logger.With("auth", "oidc")
	logger.DebugContext(r.Context(), "Console login start")

	req := new(client.SSOLoginConsoleReq)
	if err := httplib.ReadResourceJSON(r, req); err != nil {
		logger.ErrorContext(r.Context(), "Error reading json", "error", err)
		return nil, trace.AccessDenied("%s", SSOLoginFailureMessage)
	}

	if err := req.CheckAndSetDefaults(); err != nil {
		logger.ErrorContext(r.Context(), "Missing request parameters", "error", err)
		return nil, trace.AccessDenied("%s", SSOLoginFailureMessage)
	}

	remoteAddr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		logger.ErrorContext(r.Context(), "Failed to parse request remote address", "error", err)
		return nil, trace.AccessDenied("%s", SSOLoginFailureMessage)
	}

	response, err := h.cfg.ProxyClient.CreateOIDCAuthRequest(r.Context(), types.OIDCAuthRequest{
		ConnectorID:             req.ConnectorID,
		SshPublicKey:            req.SSHPubKey,
		TlsPublicKey:            req.TLSPubKey,
		SshAttestationStatement: req.SSHAttestationStatement.ToProto(),
		TlsAttestationStatement: req.TLSAttestationStatement.ToProto(),
		CertTTL:                 req.CertTTL,
		ClientRedirectURL:       req.RedirectURL,
		Compatibility:           req.Compatibility,
		RouteToCluster:          req.RouteToCluster,
		KubernetesCluster:       req.KubernetesCluster,
		ClientLoginIP:           remoteAddr,
		Scope:                   req.Scope,
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "Failed to create OIDC auth request", "error", err)
		if strings.Contains(err.Error(), auth.InvalidClientRedirectErrorMessage) {
			return nil, trace.AccessDenied("%s", SSOLoginFailureInvalidRedirect)
		}
		return nil, trace.AccessDenied("%s", SSOLoginFailureMessage)
	}

	return &client.SSOLoginConsoleResponse{
		RedirectURL: response.RedirectURL,
	}, nil
}

// oidcCallback is the IdP's redirect target. It validates the callback via the
// auth server (which delegates to the in-house OIDC service) and then either
// sets a Web session cookie or constructs an SSH-response redirect for tsh.
//
// Mirrors githubCallback.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request, p httprouter.Params) string {
	logger := h.logger.With("auth", "oidc")
	// Never log the raw query string — it carries `code` and `state`,
	// which are sensitive (auth-code reuse / CSRF lookup secret). Record
	// only presence flags + the IdP-side error code, which is safe.
	q := r.URL.Query()
	logger.DebugContext(r.Context(), "Callback start",
		"has_state", q.Get("state") != "",
		"has_code", q.Get("code") != "",
		"idp_error", q.Get("error"),
	)

	response, err := h.cfg.ProxyClient.ValidateOIDCAuthCallback(r.Context(), r.URL.Query())
	if err != nil {
		logger.ErrorContext(r.Context(), "Error while processing OIDC callback", "error", err)

		// Try to recover the original client redirect URL so the client-side
		// flow terminates immediately instead of timing out.
		if requestID := r.URL.Query().Get("state"); requestID != "" {
			if request, errGet := h.cfg.ProxyClient.GetOIDCAuthRequest(r.Context(), requestID); errGet == nil && !request.CreateWebSession {
				if redURL, errEnc := RedirectURLWithError(request.ClientRedirectURL, err); errEnc == nil {
					return redURL.String()
				}
			}
		}
		return sso.LoginFailedBadCallbackRedirectURL
	}

	if response.Req.CreateWebSession {
		logger.InfoContext(r.Context(), "Redirecting to web browser")

		res := &SSOCallbackResponse{
			CSRFToken:         response.Req.CSRFToken,
			Username:          response.Username,
			SessionName:       response.Session.GetName(),
			SessionExpiry:     response.Session.Expiry(),
			ClientRedirectURL: response.Req.ClientRedirectURL,
		}

		if err := SSOSetWebSessionAndRedirectURL(w, r, res, true); err != nil {
			logger.ErrorContext(r.Context(), "Error setting web session.", "error", err)
			return sso.LoginFailedRedirectURL
		}

		if dwt := response.Session.GetDeviceWebToken(); dwt != nil {
			redirectPath, err := BuildDeviceWebRedirectPath(dwt, res.ClientRedirectURL)
			if err != nil {
				logger.DebugContext(r.Context(), "Invalid device web token", "error", err)
			}
			return redirectPath
		}
		return res.ClientRedirectURL
	}

	logger.InfoContext(r.Context(), "Callback is redirecting to console login")
	if len(response.Req.SSHPubKey)+len(response.Req.TLSPubKey) == 0 {
		logger.ErrorContext(r.Context(), "Not a web or console login request")
		return sso.LoginFailedRedirectURL
	}

	redirectURL, err := ConstructSSHResponse(AuthParams{
		ClientRedirectURL: response.Req.ClientRedirectURL,
		Username:          response.Username,
		Identity:          response.Identity,
		Session:           response.Session,
		Cert:              response.Cert,
		TLSCert:           response.TLSCert,
		HostSigners:       response.HostSigners,
		FIPS:              h.cfg.FIPS,
		ClientOptions:     response.ClientOptions,
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "Error constructing ssh response", "error", err)
		return sso.LoginFailedRedirectURL
	}

	return redirectURL.String()
}
