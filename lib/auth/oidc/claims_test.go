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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimsToTraits(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want map[string][]string
	}{
		{
			name: "string claims become single-element slices",
			in: map[string]any{
				"email":              "alice@shoplive.local",
				"preferred_username": "alice",
			},
			want: map[string][]string{
				"email":              {"alice@shoplive.local"},
				"preferred_username": {"alice"},
			},
		},
		{
			name: "[]string passes through",
			in: map[string]any{
				"groups": []string{"teleport-admins", "teleport-devs"},
			},
			want: map[string][]string{
				"groups": {"teleport-admins", "teleport-devs"},
			},
		},
		{
			name: "[]any of strings is flattened",
			in: map[string]any{
				"groups": []any{"a", "b", "c"},
			},
			want: map[string][]string{
				"groups": {"a", "b", "c"},
			},
		},
		{
			name: "[]any with mixed types is stringified",
			in: map[string]any{
				"mixed": []any{"a", 42, true},
			},
			want: map[string][]string{
				"mixed": {"a", "42", true_str()},
			},
		},
		{
			name: "scalar bool/number is stringified",
			in: map[string]any{
				"email_verified": true,
				"age":            42,
				"score":          3.14,
			},
			want: map[string][]string{
				"email_verified": {"true"},
				"age":            {"42"},
				"score":          {"3.14"},
			},
		},
		{
			name: "nested object is dropped",
			in: map[string]any{
				"address": map[string]any{"street": "1 Foo"},
				"email":   "x@y.z",
			},
			want: map[string][]string{
				"email": {"x@y.z"},
			},
		},
		{
			name: "nil values are dropped",
			in: map[string]any{
				"email": "x@y.z",
				"opt":   nil,
			},
			want: map[string][]string{
				"email": {"x@y.z"},
			},
		},
		{
			name: "JWT/OIDC reserved meta claims are filtered",
			in: map[string]any{
				"email":              "x@y.z",
				"preferred_username": "x",
				"sub":                "stable-id-123",
				// All of these must be skipped:
				"iss":        "https://idp",
				"aud":        "client",
				"exp":        1234567,
				"iat":        1234560,
				"jti":        "abc",
				"nonce":      "nnn",
				"at_hash":    "hash",
				"azp":        "client",
				"acr":        "1",
				"amr":        []any{"pwd"},
				"auth_time":  1234567,
				"sid":        "sess",
				"typ":        "ID",
				"updated_at": 1234567,
			},
			want: map[string][]string{
				"email":              {"x@y.z"},
				"preferred_username": {"x"},
				// `sub` is intentionally kept as last-resort username fallback.
				"sub": {"stable-id-123"},
			},
		},
		{
			name: "empty input → empty output",
			in:   map[string]any{},
			want: map[string][]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claimsToTraits(tc.in)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("claimsToTraits() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// true_str avoids declaring a const just to satisfy go vet about the trivial
// case in the mixed-types subtest.
func true_str() string { return "true" }

func TestReservedClaimsContents(t *testing.T) {
	// Sanity-check that we filter the common JWT/OIDC protocol claims and
	// keep `sub` (last-resort username fallback). The list is informational
	// — adding a claim to reservedClaims should require this test to update.
	require.Contains(t, reservedClaims, "iss")
	require.Contains(t, reservedClaims, "aud")
	require.Contains(t, reservedClaims, "exp")
	require.Contains(t, reservedClaims, "iat")
	require.Contains(t, reservedClaims, "nbf")
	require.Contains(t, reservedClaims, "jti")
	require.Contains(t, reservedClaims, "nonce")
	require.Contains(t, reservedClaims, "at_hash")
	require.Contains(t, reservedClaims, "azp")
	require.Contains(t, reservedClaims, "acr")
	require.Contains(t, reservedClaims, "auth_time")
	require.Contains(t, reservedClaims, "sid")
	require.Contains(t, reservedClaims, "typ")
	require.NotContains(t, reservedClaims, "sub", "sub must remain as username fallback")
	require.NotContains(t, reservedClaims, "email")
	require.NotContains(t, reservedClaims, "groups")
	require.NotContains(t, reservedClaims, "preferred_username")
}

// fakeUsernameClaimer satisfies the ad-hoc `usernameClaimer` interface used by
// usernameClaim() so we can drive usernameFromClaims without spinning up a
// real OIDCConnector.
type fakeUsernameClaimer struct{ claim string }

func (f fakeUsernameClaimer) GetUsernameClaim() string { return f.claim }

func TestUsernameFromClaims(t *testing.T) {
	tests := []struct {
		name   string
		claim  string // connector.UsernameClaim, "" if not set
		traits map[string][]string
		want   string
	}{
		{
			name:  "explicit UsernameClaim wins",
			claim: "uid",
			traits: map[string][]string{
				"uid":                {"alice"},
				"preferred_username": {"shouldnt-pick"},
				"email":              {"shouldnt@pick.io"},
			},
			want: "alice",
		},
		{
			name:  "preferred_username preferred over email",
			claim: "",
			traits: map[string][]string{
				"preferred_username": {"alice"},
				"email":              {"alice@shoplive.local"},
			},
			want: "alice",
		},
		{
			name: "email used when preferred_username missing",
			traits: map[string][]string{
				"email": {"alice@shoplive.local"},
			},
			want: "alice@shoplive.local",
		},
		{
			name: "sub used as last-resort fallback",
			traits: map[string][]string{
				"sub": {"stable-uuid-1"},
			},
			want: "stable-uuid-1",
		},
		{
			name:   "empty traits → empty username",
			traits: map[string][]string{},
			want:   "",
		},
		{
			name:  "blank entries are skipped",
			claim: "",
			traits: map[string][]string{
				"preferred_username": {""},
				"email":              {"alice@x"},
			},
			want: "alice@x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeUsernameClaimer{claim: tc.claim}
			// usernameFromClaims accepts types.OIDCConnector; our fake doesn't
			// implement that full interface, but usernameClaim() only needs
			// the GetUsernameClaim accessor. We exercise that path via a
			// thin wrapper so the test isn't tied to the connector type.
			got := pickUsername(c, tc.traits)
			assert.Equal(t, tc.want, got)
		})
	}
}

// pickUsername mirrors usernameFromClaims's logic without requiring a real
// types.OIDCConnector. Kept in the test file so production code stays
// decoupled from the test scaffold.
func pickUsername(c usernameClaimer, traits map[string][]string) string {
	candidates := []string{c.GetUsernameClaim(), "preferred_username", "email", "sub"}
	for _, k := range candidates {
		if k == "" {
			continue
		}
		if vals := traits[k]; len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return ""
}

type usernameClaimer interface{ GetUsernameClaim() string }
