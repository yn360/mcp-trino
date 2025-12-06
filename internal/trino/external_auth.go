package trino

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ExternalAuthenticator handles Trino external authentication (browser OAuth flow)
type ExternalAuthenticator struct {
	baseURL    string
	username   string
	httpClient *http.Client
	tokenCache *tokenCache
	timeout    time.Duration
	mu         sync.Mutex // Protects concurrent access to tokenCache
}

// tokenCache holds cached OAuth tokens
type tokenCache struct {
	token     string
	expiresAt time.Time
}

// NewExternalAuthenticator creates an ExternalAuthenticator configured for the given Trino base URL and username.
// The timeoutSecs parameter specifies the maximum duration, in seconds, to wait for the external authentication flow to complete.
// The returned ExternalAuthenticator uses an HTTP client with a 30-second request timeout.
func NewExternalAuthenticator(baseURL, username string, timeoutSecs int) *ExternalAuthenticator {
	return &ExternalAuthenticator{
		baseURL:    baseURL,
		username:   username,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		timeout:    time.Duration(timeoutSecs) * time.Second,
	}
}

// GetToken retrieves a valid OAuth token, using cache if available
func (a *ExternalAuthenticator) GetToken(ctx context.Context) (string, error) {
	a.mu.Lock()

	// Check if we have a valid cached token
	if a.tokenCache != nil && time.Now().Before(a.tokenCache.expiresAt) {
		token := a.tokenCache.token
		a.mu.Unlock()
		log.Println("INFO: Using cached OAuth token")
		return token, nil
	}

	// Release lock during long-running auth flow to allow other operations
	a.mu.Unlock()

	log.Println("INFO: No valid cached token, initiating external authentication flow")

	// Trigger the external auth flow
	redirectURL, tokenURL, err := a.getAuthURLs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get auth URLs: %w", err)
	}

	log.Printf("INFO: Opening browser for authentication at: %s", redirectURL)

	// Open browser for user authentication
	if err := openBrowser(redirectURL); err != nil {
		log.Printf("WARNING: Failed to open browser automatically: %v", err)
		log.Printf("Please manually open this URL in your browser: %s", redirectURL)
	}

	// Poll for token
	log.Println("INFO: Waiting for authentication to complete...")
	token, err := a.pollForToken(ctx, tokenURL)
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}

	// Re-acquire lock to update cache
	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-check: another goroutine might have completed auth while we were waiting
	if a.tokenCache != nil && time.Now().Before(a.tokenCache.expiresAt) {
		return a.tokenCache.token, nil
	}

	// Cache the token (assume 1 hour TTL if not specified)
	a.tokenCache = &tokenCache{
		token:     token,
		expiresAt: time.Now().Add(1 * time.Hour),
	}

	log.Println("INFO: Successfully authenticated and cached token")
	return token, nil
}

// InvalidateToken clears the cached token, forcing re-authentication on next request
func (a *ExternalAuthenticator) InvalidateToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokenCache = nil
	log.Println("INFO: OAuth token cache invalidated")
}

// getAuthURLs retrieves the OAuth redirect and token URLs from Trino server
func (a *ExternalAuthenticator) getAuthURLs(ctx context.Context) (redirectURL, tokenURL string, err error) {
	// Make a request to Trino without auth to trigger 401 with OAuth URLs
	url := fmt.Sprintf("%s/v1/statement", a.baseURL)

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("SELECT 1"))
	if err != nil {
		return "", "", err
	}

	req.Header.Set("X-Trino-User", a.username)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	// We expect 401 Unauthorized with WWW-Authenticate header
	if resp.StatusCode != http.StatusUnauthorized {
		return "", "", fmt.Errorf("unexpected status code: %d (expected 401)", resp.StatusCode)
	}

	// Parse WWW-Authenticate header for OAuth URLs
	authHeader := resp.Header.Get("WWW-Authenticate")
	if authHeader == "" {
		return "", "", fmt.Errorf("no WWW-Authenticate header found")
	}

	// Parse the Bearer challenge
	// Format: Bearer x_redirect_server="...", x_token_server="..."
	redirectURL, tokenURL = parseAuthHeader(authHeader)
	if redirectURL == "" || tokenURL == "" {
		return "", "", fmt.Errorf("failed to parse OAuth URLs from header: %s", authHeader)
	}

	return redirectURL, tokenURL, nil
}

// parseAuthHeader parses the WWW-Authenticate header to verify Bearer scheme
// parseAuthHeader parses a WWW-Authenticate header for the Bearer scheme and
// extracts the `x_redirect_server` and `x_token_server` URLs.
// It returns the redirect URL and token URL as strings; either will be empty
// if the header is not a Bearer challenge or the respective key is not present.
func parseAuthHeader(header string) (redirectURL, tokenURL string) {
	// Check for Bearer scheme (case-insensitive)
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return "", ""
	}

	// Regex to extract quoted values for x_redirect_server and x_token_server
	// Handles potentially different ordering or whitespace
	reRedirect := regexp.MustCompile(`x_redirect_server\s*=\s*"([^"]+)"`)
	reToken := regexp.MustCompile(`x_token_server\s*=\s*"([^"]+)"`)

	if match := reRedirect.FindStringSubmatch(header); len(match) > 1 {
		redirectURL = match[1]
	}

	if match := reToken.FindStringSubmatch(header); len(match) > 1 {
		tokenURL = match[1]
	}

	return redirectURL, tokenURL
}

// pollForToken polls the token URL until authentication is complete
func (a *ExternalAuthenticator) pollForToken(ctx context.Context, tokenURL string) (string, error) {
	pollInterval := 5 * time.Second
	deadline := time.Now().Add(a.timeout)

	// Try immediately first (user may have already completed auth)
	token, err := a.tryGetToken(ctx, tokenURL)
	if err == nil && token != "" {
		return token, nil
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			token, err := a.tryGetToken(ctx, tokenURL)
			if err == nil && token != "" {
				return token, nil
			}
			// Continue polling on error or empty token
		}
	}

	return "", fmt.Errorf("authentication timeout: user did not complete authentication within %v", a.timeout)
}

// tryGetToken attempts to retrieve the token from the token URL
func (a *ExternalAuthenticator) tryGetToken(ctx context.Context, tokenURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 200 means token is ready
	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}

		// Parse token from response
		var tokenResp struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			// Token might be plain text
			return strings.TrimSpace(string(body)), nil
		}
		return tokenResp.Token, nil
	}

	// 404 or other codes mean not ready yet
	return "", fmt.Errorf("token not ready (status: %d)", resp.StatusCode)
}

// openBrowser opens targetURL in the system's default web browser.
// It validates that the URL parses and uses the http or https scheme, then invokes the platform-specific command to launch the browser.
// Returns an error if the URL is invalid, has an unsafe scheme, the platform is unsupported, or if starting the browser process fails.
func openBrowser(targetURL string) error {
	// Validate URL scheme for security
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsafe URL scheme: %s", parsed.Scheme)
	}

	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{targetURL}
	case "linux":
		cmd = "xdg-open"
		args = []string{targetURL}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", targetURL}
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return exec.Command(cmd, args...).Start()
}

// IsAuthenticationError checks if an error indicates authentication failure
// IsAuthenticationError reports whether err indicates an authentication failure or a
// connection problem that may require re-authentication. It checks the error string
// for common indicators such as "401", "unauthorized", "authentication", and
// connection-related phrases like "connection refused", "connection reset",
// "use of closed", or "broken pipe".
// Returns true if re-authentication may be required, false otherwise.
func IsAuthenticationError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	// Direct auth errors
	if strings.Contains(errStr, "401") ||
		strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "authentication") {
		return true
	}
	// Connection closure errors that may result from concurrent re-auth
	// These warrant a retry with fresh authentication
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "use of closed") ||
		strings.Contains(errStr, "broken pipe") {
		return true
	}
	return false
}