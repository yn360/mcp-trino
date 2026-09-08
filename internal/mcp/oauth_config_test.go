package mcp

import (
	"os"
	"reflect"
	"testing"
)

// withEnv sets an env var for the duration of the test and restores it after.
func withEnv(t *testing.T, key, value string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	if value == "" {
		_ = os.Unsetenv(key)
	} else {
		_ = os.Setenv(key, value)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestResolveFixedRedirectURI(t *testing.T) {
	tests := []struct {
		name             string
		redirectURIs     string
		explicitFixedEnv string
		want             string
	}{
		{
			name:         "single URI infers fixed-redirect mode (v1.0.1 compat)",
			redirectURIs: "https://mcp.example.com/oauth/callback",
			want:         "https://mcp.example.com/oauth/callback",
		},
		{
			name:         "single URI with surrounding whitespace is trimmed",
			redirectURIs: "  https://mcp.example.com/oauth/callback  ",
			want:         "https://mcp.example.com/oauth/callback",
		},
		{
			name:         "comma-separated list is allowlist-only, not inferred as fixed",
			redirectURIs: "https://a.example.com/cb,https://b.example.com/cb",
			want:         "",
		},
		{
			name:         "empty RedirectURIs yields no fixed URI",
			redirectURIs: "",
			want:         "",
		},
		{
			name:             "explicit OAUTH_FIXED_REDIRECT_URI wins over inference",
			redirectURIs:     "https://a.example.com/cb,https://b.example.com/cb",
			explicitFixedEnv: "https://explicit.example.com/cb",
			want:             "https://explicit.example.com/cb",
		},
		{
			name:             "explicit OAUTH_FIXED_REDIRECT_URI wins even with a single-valued RedirectURIs",
			redirectURIs:     "https://mcp.example.com/oauth/callback",
			explicitFixedEnv: "https://explicit.example.com/cb",
			want:             "https://explicit.example.com/cb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEnv(t, "OAUTH_FIXED_REDIRECT_URI", tt.explicitFixedEnv)

			got := resolveFixedRedirectURI(tt.redirectURIs)
			if got != tt.want {
				t.Errorf("resolveFixedRedirectURI(%q) = %q, want %q", tt.redirectURIs, got, tt.want)
			}
		})
	}
}

func TestResolveOIDCScopes(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want []string
	}{
		{name: "unset falls back to library default (nil)", env: "", want: nil},
		{name: "single scope", env: "openid", want: []string{"openid"}},
		{
			name: "comma-separated scopes, including offline_access for refresh",
			env:  "openid,profile,email,offline_access",
			want: []string{"openid", "profile", "email", "offline_access"},
		},
		{
			name: "whitespace around entries is trimmed",
			env:  " openid , offline_access ",
			want: []string{"openid", "offline_access"},
		},
		{
			name: "empty entries from stray commas are dropped",
			env:  "openid,,offline_access,",
			want: []string{"openid", "offline_access"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEnv(t, "OIDC_SCOPES", tt.env)

			got := resolveOIDCScopes()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resolveOIDCScopes() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
