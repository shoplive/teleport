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
	"fmt"

	"github.com/gravitational/teleport/api/types"
)

// reservedClaims are JWT/OIDC protocol claims that have no business polluting
// `user.spec.traits`. They're either signed-token bookkeeping (`iss`, `aud`,
// `exp`, ...) or correlation IDs (`jti`, `sid`, `nonce`) and matching them in
// role templates / TraitsToRoles is at best meaningless and at worst a
// footgun. Filtered before claimsToTraits emits into Teleport.
//
// Kept claims (groups, email, name, given_name, family_name,
// preferred_username, locale, etc.) flow through unchanged.
var reservedClaims = map[string]struct{}{
	"iss":        {}, // issuer
	"aud":        {}, // audience
	"exp":        {}, // expiry
	"iat":        {}, // issued-at
	"nbf":        {}, // not-before
	"jti":        {}, // JWT ID
	// `sub` (subject) intentionally NOT filtered: it's the IdP-stable user
	// identifier and is the documented last-resort fallback in
	// usernameFromClaims when preferred_username and email are absent.
	"nonce":      {}, // CSRF/replay defense, request-scoped
	"at_hash":    {}, // access-token hash
	"c_hash":     {}, // code hash
	"azp":        {}, // authorized party
	"acr":        {}, // auth context class
	"amr":        {}, // auth methods
	"auth_time":  {}, // auth timestamp
	"sid":        {}, // session ID
	"typ":        {}, // token type
	"updated_at": {}, // last profile update
}

// claimsToTraits flattens an OIDC ID-token / userinfo claim set into the
// Teleport trait shape — `map[string][]string` — that the role / login-rule
// machinery expects. Strings become single-element slices; arrays of strings
// pass through; everything else is stringified via fmt.Sprintf("%v") so
// integer / boolean claims are still mappable. Nested objects are skipped.
//
// Reserved JWT/OIDC protocol claims (see reservedClaims) are dropped.
func claimsToTraits(claims map[string]any) map[string][]string {
	out := make(map[string][]string, len(claims))
	for k, v := range claims {
		if _, reserved := reservedClaims[k]; reserved {
			continue
		}
		switch val := v.(type) {
		case string:
			out[k] = []string{val}
		case []string:
			out[k] = append([]string(nil), val...)
		case []any:
			ss := make([]string, 0, len(val))
			for _, item := range val {
				if s, ok := item.(string); ok {
					ss = append(ss, s)
					continue
				}
				ss = append(ss, fmt.Sprintf("%v", item))
			}
			out[k] = ss
		case bool, float64, int, int64:
			out[k] = []string{fmt.Sprintf("%v", val)}
		default:
			// Skip nested objects, nulls, etc.
		}
	}
	return out
}

// usernameFromClaims picks a stable username for the Teleport user record.
// The connector may declare a UsernameClaim (e.g. "preferred_username");
// otherwise we fall back to "email", then "sub". Returning "" forces the
// caller to error out — we never want an empty username.
func usernameFromClaims(c types.OIDCConnector, traits map[string][]string) string {
	candidates := []string{usernameClaim(c), "preferred_username", "email", "sub"}
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

// usernameClaim reads connector.Spec.UsernameClaim if present.
func usernameClaim(c types.OIDCConnector) string {
	type usernameClaimer interface{ GetUsernameClaim() string }
	if v, ok := c.(usernameClaimer); ok {
		return v.GetUsernameClaim()
	}
	return ""
}
