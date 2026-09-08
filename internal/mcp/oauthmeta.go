package mcp

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// registerOAuthHandlers registers the oauth-mcp-proxy routes on mux, patching
// the authorization-server discovery responses to advertise the refresh_token
// grant.
//
// oauth-mcp-proxy's /oauth/token has accepted grant_type=refresh_token since
// v1.1.0 (see its handlers.go HandleToken), but its discovery metadata still
// advertises only ["authorization_code"] in grant_types_supported, so RFC 8414
// conformant clients skip silent renewal and force a full re-auth on every
// access-token expiry. Upstream fix: https://github.com/tuannvm/oauth-mcp-proxy/pull/34
// (open since 2026-06-02, unmerged). Delete this shim once that ships and we've
// bumped past it.
//
// RegisterHandlers writes all of its routes onto a single mux and the
// *OAuth2Handler it wires up is unexported, so we can't just call one patched
// handler in place of one route. Instead we let the library register onto an
// inner mux, then shadow only the two discovery routes with a patched wrapper.
func (s *Server) registerOAuthHandlers(mux *http.ServeMux) {
	inner := http.NewServeMux()
	s.oauthServer.RegisterHandlers(inner)

	patched := addRefreshTokenGrant(inner)
	mux.Handle("/.well-known/oauth-authorization-server", patched)
	mux.Handle("/.well-known/openid-configuration", patched)

	// Everything else the library registered (protected-resource metadata,
	// jwks, authorize/callback/token/register), unmodified.
	mux.Handle("/.well-known/oauth-protected-resource", inner)
	mux.Handle("/.well-known/jwks.json", inner)
	mux.Handle("/oauth/", inner)
}

// addRefreshTokenGrant wraps next, buffering its response and adding
// "refresh_token" to a top-level "grant_types_supported" JSON array if the
// response is a 200 with that field and it's missing the entry. Any failure
// to parse falls back to passing the original response through untouched -
// this shim must never be able to break OAuth discovery.
func addRefreshTokenGrant(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{header: make(http.Header), statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		body := rec.body.Bytes()
		// OPTIONS/HEAD preflight responses have no body - nothing to patch,
		// and not worth a log line.
		if rec.statusCode == http.StatusOK && len(body) > 0 {
			if patchedBody, ok := patchGrantTypes(body); ok {
				body = patchedBody
			} else {
				log.Printf("WARN: OAuth metadata shim could not parse response from %s; passing through unmodified", r.URL.Path)
			}
		}

		for k, values := range rec.header {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.statusCode)
		_, _ = w.Write(body)
	}
}

// patchGrantTypes adds "refresh_token" to a top-level grant_types_supported
// array in the given JSON document, if present and not already there. The
// second return value is false if body isn't a JSON object with that field,
// in which case the caller should use the original body unchanged.
func patchGrantTypes(body []byte) ([]byte, bool) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}

	raw, ok := doc["grant_types_supported"]
	if !ok {
		return nil, false
	}
	grants, ok := raw.([]interface{})
	if !ok {
		return nil, false
	}

	for _, g := range grants {
		if s, ok := g.(string); ok && s == "refresh_token" {
			return body, true // already present, nothing to do
		}
	}

	doc["grant_types_supported"] = append(grants, "refresh_token")

	patched, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return patched, true
}

// responseRecorder is a minimal http.ResponseWriter that buffers the body
// instead of writing it, so it can be inspected and rewritten before being
// sent to the real client.
type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(b)
}
