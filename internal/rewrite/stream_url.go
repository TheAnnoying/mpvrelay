// Package rewrite is the only code in this system that interprets IPC
// content: rewriting a "loadfile" path into a mediastream URL the client
// can fetch (BuildStreamURL), and rewriting the "path"/"filename" it
// reports back so Seanime's exact-string local-file matching still finds
// the file (see ExtractPathFromStreamURL - the real path travels in the
// URL's own query string rather than as tracked session state, so it
// can't desync when two loadfiles are in flight at once).
package rewrite

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// MediastreamPath is Seanime's own file-serving endpoint, reused here
// instead of running a bespoke HTTP server: it already handles Range
// requests, library-path validation and HMAC auth.
const MediastreamPath = "/api/v1/mediastream/file"

// defaultHMACSecret mirrors Seanime's GetServerPasswordHMACAuth: used
// when no server password is configured.
const defaultHMACSecret = "seanime-default-secret"

// tokenTTL mirrors the TTL Seanime itself uses for this token type.
const tokenTTL = 24 * time.Hour

// tokenClaims mirrors Seanime's TokenClaims exactly - field names and
// JSON tags included - since the signature covers these encoded bytes
// verbatim.
type tokenClaims struct {
	Endpoint  string `json:"endpoint"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func b64url(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// hmacSecret reproduces Seanime's secret derivation: sha256-hex of the
// plaintext server password, or a fixed default when unconfigured.
func hmacSecret(serverPassword string) string {
	if serverPassword == "" {
		return defaultHMACSecret
	}
	sum := sha256.Sum256([]byte(serverPassword))
	return hex.EncodeToString(sum[:])
}

// MintToken reproduces Seanime's HMACAuth.GenerateToken from outside the
// Seanime process (this tool has no dependency on Seanime's module
// graph); it must stay in lockstep with that function.
func MintToken(serverPassword, endpoint string) (string, error) {
	now := time.Now().Unix()
	claims := tokenClaims{
		Endpoint:  endpoint,
		IssuedAt:  now,
		ExpiresAt: now + int64(tokenTTL.Seconds()),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("rewrite: marshal token claims: %w", err)
	}
	claimsB64 := b64url(claimsJSON)

	mac := hmac.New(sha256.New, []byte(hmacSecret(serverPassword)))
	mac.Write([]byte(claimsB64))
	sig := b64url(mac.Sum(nil))

	return claimsB64 + "." + sig, nil
}

// BuildStreamURL builds the URL the client's real mpv should open in
// place of realAbsPath. serverBaseURL must be reachable from the client
// machine (e.g. "http://myserver.local:43211").
func BuildStreamURL(serverBaseURL, serverPassword, realAbsPath string) (string, error) {
	base := strings.TrimRight(serverBaseURL, "/")
	u, err := url.Parse(base + MediastreamPath)
	if err != nil {
		return "", fmt.Errorf("rewrite: invalid server base URL %q: %w", serverBaseURL, err)
	}

	token, err := MintToken(serverPassword, MediastreamPath)
	if err != nil {
		return "", err
	}

	q := url.Values{}
	// Standard (not URL-safe) base64, matching what Seanime's endpoint decodes.
	q.Set("path", base64.StdEncoding.EncodeToString([]byte(realAbsPath)))
	q.Set("token", token)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// IsMediaURL reports whether mediaPath is already a URL Seanime wants
// mpv to open directly - torrent streaming, which points at Seanime's
// own embedded torrent-stream server - rather than a real absolute
// filesystem path that needs BuildStreamURL.
func IsMediaURL(mediaPath string) bool {
	u, err := url.Parse(mediaPath)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// RewriteTorrentStreamURL rewrites a Seanime torrent-stream URL's host.
// Seanime binds that embedded server to a loopback address, meaningful
// only on the server's own machine, and its port is the one inside the
// container, so this swaps in the server's LAN scheme, host and port
// (from serverBaseURL) while keeping the original path and query - including Seanime's own already-valid token - untouched.
// title is a display name for mpv's window, derived from the URL path.
func RewriteTorrentStreamURL(serverBaseURL, mediaURL string) (rewrittenURL, title string, err error) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return "", "", fmt.Errorf("rewrite: invalid torrent-stream URL %q: %w", mediaURL, err)
	}
	base, err := url.Parse(serverBaseURL)
	if err != nil {
		return "", "", fmt.Errorf("rewrite: invalid server base URL %q: %w", serverBaseURL, err)
	}

	u.Scheme = base.Scheme
	u.Host = base.Host

	return u.String(), path.Base(u.Path), nil
}

// ExtractPathFromStreamURL is BuildStreamURL's inverse. ok is false for
// any value that isn't one of our URLs (e.g. mpv hasn't loaded anything
// yet). Deliberately matched by query params alone ("token" + "path"),
// not a MediastreamPath suffix: mpv's "filename" property truncates a
// loaded URL down to its last path segment plus query string, dropping
// any path prefix.
func ExtractPathFromStreamURL(raw string) (path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	q := u.Query()
	if q.Get("token") == "" {
		return "", false
	}
	enc := q.Get("path")
	if enc == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}
