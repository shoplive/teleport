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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestParseCallbackParams(t *testing.T) {
	tests := []struct {
		name      string
		q         url.Values
		wantState string
		wantCode  string
		assertErr func(t *testing.T, err error)
	}{
		{
			name:      "happy path",
			q:         url.Values{"state": {"s1"}, "code": {"c1"}},
			wantState: "s1",
			wantCode:  "c1",
			assertErr: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			name: "idp error surfaces as AccessDenied",
			q: url.Values{
				"error":             {"access_denied"},
				"error_description": {"user cancelled at IdP"},
				// state/code present but the IdP error must win.
				"state": {"s1"},
				"code":  {"c1"},
			},
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, trace.IsAccessDenied(err),
					"want AccessDenied, got %T: %v", trace.Unwrap(err), err)
				assert.Contains(t, err.Error(), "access_denied")
				assert.Contains(t, err.Error(), "user cancelled at IdP")
			},
		},
		{
			name: "missing state is BadParameter",
			q:    url.Values{"code": {"c1"}},
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, trace.IsBadParameter(err),
					"want BadParameter, got %T: %v", trace.Unwrap(err), err)
				assert.Contains(t, err.Error(), "state")
			},
		},
		{
			name: "missing code is BadParameter",
			q:    url.Values{"state": {"s1"}},
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, trace.IsBadParameter(err),
					"want BadParameter, got %T: %v", trace.Unwrap(err), err)
				assert.Contains(t, err.Error(), "code")
			},
		},
		{
			name: "empty error_description is still AccessDenied",
			q:    url.Values{"error": {"server_error"}},
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, trace.IsAccessDenied(err))
				assert.Contains(t, err.Error(), "server_error")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, code, err := parseCallbackParams(tc.q)
			tc.assertErr(t, err)
			if err == nil {
				assert.Equal(t, tc.wantState, state)
				assert.Equal(t, tc.wantCode, code)
			} else {
				assert.Empty(t, state)
				assert.Empty(t, code)
			}
		})
	}
}

func TestVerifyMFAUsernameMatch(t *testing.T) {
	t.Run("match returns nil", func(t *testing.T) {
		require.NoError(t, verifyMFAUsernameMatch("alice", "alice"))
	})

	t.Run("mismatch returns AccessDenied with both names", func(t *testing.T) {
		err := verifyMFAUsernameMatch("alice", "mallory")
		require.Error(t, err)
		assert.True(t, trace.IsAccessDenied(err),
			"want AccessDenied, got %T: %v", trace.Unwrap(err), err)
		assert.Contains(t, err.Error(), "OIDC MFA user mismatch")
		assert.Contains(t, err.Error(), "alice")
		assert.Contains(t, err.Error(), "mallory")
	})

	t.Run("empty session expectation against any IdP value mismatches", func(t *testing.T) {
		err := verifyMFAUsernameMatch("", "alice")
		require.Error(t, err)
		assert.True(t, trace.IsAccessDenied(err))
	})
}

// userinfoIdP extends the fakeIdP minimal scaffolding with a configurable
// /userinfo endpoint so we can drive mergeUserInfo end-to-end against a real
// *oidc.Provider obtained via oidc.NewProvider.
type userinfoIdP struct {
	*fakeIdP
	userinfoStatus atomic.Int32 // 0 → default 200
	userinfoBody   atomic.Pointer[map[string]any]
	userinfoHits   atomic.Int64
}

func newUserinfoIdP(t *testing.T) *userinfoIdP {
	t.Helper()
	base := newFakeIdP(t)

	uip := &userinfoIdP{fakeIdP: base}

	// The /userinfo handler lives on the same mux as discovery / jwks. We
	// reach the mux via the underlying server's Handler (httptest sets a
	// *http.ServeMux there). Re-registering a path on the same mux panics,
	// so we install a top-level "/userinfo" handler exactly once.
	mux := base.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		uip.userinfoHits.Add(1)
		if status := uip.userinfoStatus.Load(); status != 0 {
			http.Error(w, http.StatusText(int(status)), int(status))
			return
		}
		body := uip.userinfoBody.Load()
		w.Header().Set("Content-Type", "application/json")
		if body == nil {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(*body)
	})

	return uip
}

// setUserinfo sets the JSON body returned by the /userinfo endpoint. The
// userinfo claims MUST include "sub" — go-oidc cross-checks the userinfo
// "sub" against the ID-token "sub", and our test always exercises the path
// without an ID token bound (no SubjectExpected), so we just ensure "sub"
// is present.
func (u *userinfoIdP) setUserinfo(body map[string]any) {
	cp := make(map[string]any, len(body))
	for k, v := range body {
		cp[k] = v
	}
	u.userinfoBody.Store(&cp)
}

func (u *userinfoIdP) setUserinfoStatus(status int) {
	u.userinfoStatus.Store(int32(status))
}

func TestMergeUserInfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	idp := newUserinfoIdP(t)
	provider, err := oidc.NewProvider(ctx, idp.srv.URL)
	require.NoError(t, err, "go-oidc must accept fake IdP discovery doc")

	// A token suffices for oauth2.StaticTokenSource — the fake /userinfo
	// handler doesn't validate the Bearer header. We just need a non-expired
	// token so the source returns it directly.
	tok := &oauth2.Token{
		AccessToken: "fake-access-token",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(1 * time.Hour),
	}

	t.Run("userinfo adds new keys but does not overwrite existing", func(t *testing.T) {
		idp.setUserinfoStatus(0) // 200 OK
		idp.setUserinfo(map[string]any{
			"sub":    "user-1",
			"email":  "should-not-overwrite@idp.example", // already in id_token claims
			"groups": []any{"engineering", "sso-admin"},  // new key
			"locale": "ko-KR",                            // new key
		})

		claims := map[string]any{
			"sub":   "user-1",
			"email": "alice@shoplive.local", // id_token wins
		}
		require.NoError(t, mergeUserInfo(ctx, provider, tok, &claims))

		assert.Equal(t, "alice@shoplive.local", claims["email"],
			"userinfo must not overwrite id_token claim")
		assert.ElementsMatch(t,
			[]any{"engineering", "sso-admin"},
			claims["groups"], "userinfo must add new key")
		assert.Equal(t, "ko-KR", claims["locale"],
			"userinfo must add new key")
	})

	t.Run("userinfo 5xx returns an error", func(t *testing.T) {
		idp.setUserinfoStatus(http.StatusInternalServerError)
		t.Cleanup(func() { idp.setUserinfoStatus(0) })

		claims := map[string]any{"sub": "user-1"}
		err := mergeUserInfo(ctx, provider, tok, &claims)
		require.Error(t, err, "5xx from userinfo must surface as an error")
		// caller (validateCallback) is documented to demote this to a warn —
		// we don't assert a specific trace.Is*, just that something came back.
		assert.True(t, strings.Contains(err.Error(), "500") ||
			strings.Contains(err.Error(), "userinfo") ||
			strings.Contains(strings.ToLower(err.Error()), "internal"),
			"error should mention 500/userinfo/internal, got: %v", err)
	})

	t.Run("userinfo with no new keys is a no-op", func(t *testing.T) {
		idp.setUserinfoStatus(0)
		idp.setUserinfo(map[string]any{
			"sub":   "user-1",
			"email": "alice-userinfo@x", // claims already have email
		})

		claims := map[string]any{
			"sub":   "user-1",
			"email": "alice@shoplive.local",
		}
		require.NoError(t, mergeUserInfo(ctx, provider, tok, &claims))

		require.Len(t, claims, 2)
		assert.Equal(t, "alice@shoplive.local", claims["email"])
		assert.Equal(t, "user-1", claims["sub"])
	})
}
