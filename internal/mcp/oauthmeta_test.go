package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	oauth "github.com/tuannvm/oauth-mcp-proxy"

	"github.com/tuannvm/mcp-trino/internal/config"
)

// newProxyModeTestServer mirrors a production proxy-mode deployment (the
// mode the reported bug was observed in) closely enough to exercise the
// metadata shim's proxy-branch fields.
func newProxyModeTestServer(t *testing.T) *Server {
	t.Helper()

	oauthServer, err := oauth.NewServer(&oauth.Config{
		Mode:         "proxy",
		Provider:     "hmac",
		Audience:     testAudience,
		ClientID:     "test-client",
		ServerURL:    "https://test-server.example.com",
		RedirectURIs: "https://test-server.example.com/oauth/callback",
		JWTSecret:    []byte(testJWTSecret),
	})
	if err != nil {
		t.Fatalf("oauth.NewServer failed: %v", err)
	}

	return &Server{
		mcpServer:   mcpserver.NewMCPServer("test", "0.0.0"),
		config:      &config.TrinoConfig{OAuthEnabled: true},
		version:     "0.0.0",
		oauthServer: oauthServer,
	}
}

func TestMetadataAdvertisesRefreshToken(t *testing.T) {
	s := newProxyModeTestServer(t)
	mux := http.NewServeMux()
	s.registerOAuthHandlers(mux)

	for _, path := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 from %s, got %d: %s", path, w.Code, w.Body.String())
			}

			var doc map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatalf("invalid JSON from %s: %v", path, err)
			}

			grants, ok := doc["grant_types_supported"].([]interface{})
			if !ok {
				t.Fatalf("%s: grant_types_supported missing or wrong type: %v", path, doc["grant_types_supported"])
			}

			var hasAuthCode, hasRefresh bool
			for _, g := range grants {
				switch g {
				case "authorization_code":
					hasAuthCode = true
				case "refresh_token":
					hasRefresh = true
				}
			}
			if !hasAuthCode {
				t.Errorf("%s: expected authorization_code in grant_types_supported, got %v", path, grants)
			}
			if !hasRefresh {
				t.Errorf("%s: expected refresh_token in grant_types_supported (this is the bug being fixed), got %v", path, grants)
			}
		})
	}
}

func TestMetadataShimPassesThroughOtherFields(t *testing.T) {
	s := newProxyModeTestServer(t)
	mux := http.NewServeMux()
	s.registerOAuthHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	for _, field := range []string{"issuer", "token_endpoint", "authorization_endpoint", "registration_endpoint"} {
		if _, ok := doc[field]; !ok {
			t.Errorf("expected %q to survive the metadata shim untouched, got: %v", field, doc)
		}
	}

	// Content-Length must match the (patched) body, not the original.
	if cl := w.Header().Get("Content-Length"); cl != "" {
		if want := strconv.Itoa(w.Body.Len()); cl != want {
			t.Errorf("Content-Length %s does not match actual body length %s", cl, want)
		}
	}
}

func TestMetadataShimPassesThroughOptionsPreflight(t *testing.T) {
	// OPTIONS preflight has no body - the shim must not choke on it or drop
	// the response.
	s := newProxyModeTestServer(t)
	mux := http.NewServeMux()
	s.registerOAuthHandlers(mux)

	req := httptest.NewRequest(http.MethodOptions, "/.well-known/oauth-authorization-server", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected OPTIONS preflight to pass through the shim, got %d", w.Code)
	}
}

func TestMetadataShimLeavesOtherRoutesUnmodified(t *testing.T) {
	// /.well-known/oauth-protected-resource is not a grant_types_supported
	// document; the shim must leave it (and everything else registered by
	// the library) alone.
	s := newProxyModeTestServer(t)
	mux := http.NewServeMux()
	s.registerOAuthHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := doc["resource"]; !ok {
		t.Errorf("expected protected-resource metadata to pass through unmodified, got: %v", doc)
	}
}
