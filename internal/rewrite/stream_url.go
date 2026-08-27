// Package rewrite contains the only code in this whole system that
// interprets the content of mpv's JSON-IPC traffic. Everything else in
// both binaries treats IPC lines as opaque bytes and forwards them
// byte-for-byte; only two things need to be understood here:
//
//  1. When Seanime sends a "loadfile <realAbsolutePath> <mode>" command,
//     the real, local filesystem path has to become a URL the Windows
//     client's real mpv can actually fetch, since the client has no
//     access whatsoever to the server's library filesystem (no share, no
//     mapped drive). See BuildStreamURL.
//
//  2. Seanime's own local-file/library matching (see
//     internal/library/playbackmanager/utils.go's getLocalFilePlaybackDetails,
//     and internal/mediaplayers/mpv/mpv.go's observe_property(46, "path"))
//     keys exclusively off mpv's "path" property, compared with plain
//     string equality after util.NormalizePath (which on Linux is just
//     filepath.ToSlash - i.e. an exact match is required). Once mpv is
//     playing a URL instead of the real path, "path" reports that URL,
//     and Seanime fails to find the local file ("local file not found").
//     So every "path" property-change event flowing back from the real
//     mpv has to be rewritten back to the exact original absolute path.
//     "filename" is rewritten too (to filepath.Base of that path) - not
//     because Seanime's matching needs it (it doesn't: matching uses
//     "path" only, confirmed by reading getLocalFilePlaybackDetails),
//     but because "filename" is what the Seanime UI displays to the user
//     as the currently-playing file, and because
//     internal/mediaplayers/mediaplayer/repository.go treats a changed
//     Filename as "a new video started playing" - a stable, real-looking
//     name avoids surprising cosmetic glitches there.
//
// The reverse mapping (event.Data -> original path) is stateless by
// construction: rather than tracking "the currently loaded file" as
// mutable session state (which breaks the moment two loadfiles are
// in flight at once, e.g. Append() pre-queuing the next episode while
// the current one is still playing), the real absolute path is encoded
// directly in the URL's own query string. Recovering it is just parsing
// that URL back apart - see ExtractPathFromStreamURL. Whatever mpv
// echoes back, right or wrong, carries its own ground truth with it.
package rewrite

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// MediastreamPath is the Seanime API endpoint this tool reuses to serve
// local library files to the remote client, instead of running a bespoke
// HTTP file server. It already does everything a bespoke server would
// have needed to reimplement:
//
//   - Range request support for seeking (it's `echo.Context.File`, i.e.
//     Go's http.ServeContent under the hood).
//   - Path validation against the server's configured library
//     directories (see internal/handlers/mediastream.go
//     HandleMediastreamFile -> mediastream.Repository.ServeEchoFile,
//     which rejects any path not under a configured library path).
//   - A Content-Disposition header carrying the real filename.
//   - Authentication via Seanime's existing HMAC query-token scheme
//     (internal/handlers/server_auth_middleware.go OptionalAuthMiddleware,
//     internal/util/hmac_auth.go), which this package re-implements just
//     enough of to mint (not merely validate) a token from outside the
//     Seanime process - see MintToken.
const MediastreamPath = "/api/v1/mediastream/file"

// defaultHMACSecret mirrors internal/core/hmac_auth.go
// App.GetServerPasswordHMACAuth: when no server password is configured,
// Seanime signs/validates these tokens with this fixed string instead of
// a hash of the password.
const defaultHMACSecret = "seanime-default-secret"

// tokenTTL mirrors the 24-hour TTL GetServerPasswordHMACAuth constructs
// its util.HMACAuth with. It only needs to outlive a single playback
// session, but there's no reason to make it shorter than what Seanime
// itself uses for the same kind of token.
const tokenTTL = 24 * time.Hour

// tokenClaims mirrors internal/util/hmac_auth.go TokenClaims exactly -
// field names, JSON tags and all - because the signature covers the
// base64url-encoded JSON bytes verbatim, not some canonical form of it.
type tokenClaims struct {
	Endpoint  string `json:"endpoint"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func b64url(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// hmacSecret reproduces internal/core/hmac_auth.go's
// App.GetServerPasswordHMACAuth secret derivation: sha256-hex of the
// plaintext server password, or a fixed default when no password is
// configured.
func hmacSecret(serverPassword string) string {
	if serverPassword == "" {
		return defaultHMACSecret
	}
	sum := sha256.Sum256([]byte(serverPassword))
	return hex.EncodeToString(sum[:])
}

// MintToken reproduces internal/util/hmac_auth.go HMACAuth.GenerateToken
// from outside the Seanime process, given only the plaintext server
// password (or "" if the server has none configured). It must stay in
// lockstep with that function; it is not calling into Seanime's code
// because this tool intentionally has no dependency on Seanime's (large)
// module graph.
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
// place of realAbsPath, pointing at the server's own mediastream/file
// endpoint. serverBaseURL is the LAN-reachable base URL of the Seanime
// server (e.g. "http://myserver.local:43211") - it must be reachable from
// the Windows client, not just from inside the server's own container.
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
	// Standard base64 (not URL-safe): this matches util.Base64EncodeStr,
	// which internal/handlers/mediastream.go's HandleMediastreamFile
	// decodes via util.IsBase64 + util.Base64DecodeStr. url.Values.Encode
	// percent-encodes '+', '/' and '=' for us, so the raw path's spaces,
	// brackets and parentheses never need any escaping of their own -
	// they're inside the base64 alphabet's input, not its output.
	q.Set("path", base64.StdEncoding.EncodeToString([]byte(realAbsPath)))
	q.Set("token", token)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// ExtractPathFromStreamURL is BuildStreamURL's inverse: given a value
// mpv reports back (e.g. a "path" or "filename" property-change), it
// checks whether that value is one of our own mediastream URLs and, if
// so, recovers the original real absolute path encoded in its query
// string. ok is false for any value that isn't one of our URLs (e.g. mpv
// hasn't loaded anything, or reports something unexpected) - callers
// should leave the property-change event untouched in that case.
//
// This deliberately does NOT require the URL's path to end in
// MediastreamPath. mpv's "path" property does report the full URL, but
// its "filename" property does not — confirmed against a real mpv.exe:
// for a URL, "filename" is truncated to just the last path segment plus
// the query string (e.g. "file?path=...&token=..." rather than
// "http://host:port/api/v1/mediastream/file?path=...&token=..."), which
// has no "/api/v1/mediastream/file" suffix left to match. The query
// string survives that truncation intact (the '?' is always inside the
// final segment), so this instead recognizes our URLs by their query
// parameters alone: a decodable "path" param plus a non-empty "token"
// param (the latter mostly to avoid a coincidental "path=" query param
// on some unrelated URL being treated as ours).
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
