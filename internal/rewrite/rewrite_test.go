package rewrite

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const testPath = `/media/Boku no Hero Academia 7/[Judas] Boku no Hero Academia (My Hero Academia) (Season 7) [1080p][HEVC x265 10bit][Dual-Audio][Multi-Subs]/[Judas] Boku no Hero Academia - S07E08v2.mkv`

func TestBuildAndExtractStreamURL_RoundTrip(t *testing.T) {
	for _, password := range []string{"", "correct horse battery staple"} {
		u, err := BuildStreamURL("http://myserver.local:43211", password, testPath)
		if err != nil {
			t.Fatalf("BuildStreamURL: %v", err)
		}

		got, ok := ExtractPathFromStreamURL(u)
		if !ok {
			t.Fatalf("ExtractPathFromStreamURL(%q) reported not-ours", u)
		}
		if got != testPath {
			t.Fatalf("round trip mismatch:\n got  %q\n want %q", got, testPath)
		}
	}
}

// TestExtractPathFromStreamURL_HandlesMpvFilenameTruncation pins down a
// real behavior observed from an actual mpv.exe in manual end-to-end
// testing: unlike "path" (which reports the full URL), mpv's "filename"
// property for a URL is truncated to just the last path segment plus the
// query string - e.g. "file?path=...&token=..." rather than
// ".../api/v1/mediastream/file?path=...&token=...". Losing the
// "/api/v1/mediastream/file" prefix must not stop extraction from
// working, since the query string (where the real path actually lives)
// survives the truncation intact.
func TestExtractPathFromStreamURL_HandlesMpvFilenameTruncation(t *testing.T) {
	u, err := BuildStreamURL("http://myserver.local:43211", "hunter2", testPath)
	if err != nil {
		t.Fatalf("BuildStreamURL: %v", err)
	}
	truncated := "file" + u[strings.IndexByte(u, '?'):]

	got, ok := ExtractPathFromStreamURL(truncated)
	if !ok {
		t.Fatalf("ExtractPathFromStreamURL(%q) reported not-ours", truncated)
	}
	if got != testPath {
		t.Fatalf("got %q, want %q", got, testPath)
	}
}

func TestExtractPathFromStreamURL_RejectsForeignValues(t *testing.T) {
	cases := []string{
		"",
		"not a url at all",
		"http://myserver.local:43211/api/v1/mediastream/file", // no path= query param
		"http://myserver.local:43211/api/v1/other/endpoint?path=" + "Zm9v",
		testPath, // a bare real path is not a URL we produced
	}
	for _, c := range cases {
		if _, ok := ExtractPathFromStreamURL(c); ok {
			t.Errorf("ExtractPathFromStreamURL(%q) unexpectedly reported ours", c)
		}
	}
}

func TestMintToken_MatchesSeanimeValidationShape(t *testing.T) {
	// This doesn't call into Seanime's util.HMACAuth (this module has no
	// dependency on the Seanime module), but it pins the wire format so
	// a future edit here can't silently drift from
	// internal/util/hmac_auth.go without a test noticing the shape
	// change: two base64url segments joined by a literal dot, the first
	// decoding to the expected claims JSON.
	token, err := MintToken("hunter2", MediastreamPath)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	dot := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		t.Fatalf("token %q has no signature separator", token)
	}
	claimsB64 := token[:dot]

	decoded, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(claimsB64)
	if err != nil {
		t.Fatalf("decoding claims segment: %v", err)
	}
	var claims tokenClaims
	if err := json.Unmarshal(decoded, &claims); err != nil {
		t.Fatalf("unmarshalling claims: %v", err)
	}
	if claims.Endpoint != MediastreamPath {
		t.Errorf("claims.Endpoint = %q, want %q", claims.Endpoint, MediastreamPath)
	}
	if claims.ExpiresAt <= claims.IssuedAt {
		t.Errorf("claims.ExpiresAt (%d) should be after IssuedAt (%d)", claims.ExpiresAt, claims.IssuedAt)
	}
}

func TestIPCRewriter_LoadfileThenPropertyChangeRoundTrip(t *testing.T) {
	rw := NewIPCRewriter("http://myserver.local:43211", "hunter2")

	// Seanime observing "path" and "filename" under the IDs it actually
	// uses today (internal/mediaplayers/mpv/mpv.go), before any file is
	// loaded. These must pass through untouched.
	for _, cmd := range []string{
		`{"command":["observe_property",46,"path"],"request_id":1}`,
		`{"command":["observe_property",45,"filename"],"request_id":2}`,
	} {
		if got := rw.RewriteOutgoing([]byte(cmd)); string(got) != cmd {
			t.Errorf("observe_property should pass through unmodified: got %q", got)
		}
	}

	// Seanime replacing the file mid-session (the loadfile/replace path
	// OpenAndPlay takes when mpv is already running).
	loadfile := `{"command":["loadfile",` + jsonStr(testPath) + `,"replace"],"request_id":3}`
	rewritten := rw.RewriteOutgoing([]byte(loadfile))

	var req struct {
		Command   []interface{} `json:"command"`
		RequestID uint          `json:"request_id"`
	}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("rewritten loadfile is not valid JSON: %v (%s)", err, rewritten)
	}
	if req.RequestID != 3 {
		t.Errorf("request_id should be preserved, got %d", req.RequestID)
	}
	streamURL, ok := req.Command[1].(string)
	if !ok {
		t.Fatalf("rewritten loadfile path arg is not a string: %+v", req.Command)
	}
	if streamURL == testPath {
		t.Fatalf("loadfile path was not rewritten at all")
	}
	if recovered, ok := ExtractPathFromStreamURL(streamURL); !ok || recovered != testPath {
		t.Fatalf("rewritten URL does not round-trip back to the real path: %q", streamURL)
	}

	// The real mpv on the client now reports its "path" and "filename"
	// properties as that URL. Both must come back rewritten: "path" to
	// the exact original absolute path (this is what Seanime's local
	// file matching keys off), "filename" to its basename (cosmetic).
	pathEvent := `{"event":"property-change","id":46,"name":"path","data":` + jsonStr(streamURL) + `}`
	rewrittenPath := rw.RewriteIncoming([]byte(pathEvent))
	assertDataField(t, rewrittenPath, testPath)

	filenameEvent := `{"event":"property-change","id":45,"name":"filename","data":` + jsonStr(streamURL) + `}`
	rewrittenFilename := rw.RewriteIncoming([]byte(filenameEvent))
	assertDataField(t, rewrittenFilename, filepath.Base(testPath))

	// A property-change for something this package doesn't touch must
	// be forwarded byte-for-byte, not merely "equivalent JSON".
	pauseEvent := `{"event":"property-change","id":43,"name":"pause","data":true}`
	if got := rw.RewriteIncoming([]byte(pauseEvent)); string(got) != pauseEvent {
		t.Errorf("unrelated property-change must pass through unmodified, got %q", got)
	}

	// Before any file is loaded, mpv reports "path"/"filename" as null.
	// That must also pass through untouched rather than panicking or
	// producing a bogus rewrite.
	nullEvent := `{"event":"property-change","id":46,"name":"path","data":null}`
	if got := rw.RewriteIncoming([]byte(nullEvent)); string(got) != nullEvent {
		t.Errorf("null path/filename must pass through unmodified, got %q", got)
	}
}

func TestIPCRewriter_IDFallbackWhenNameFieldAbsent(t *testing.T) {
	rw := NewIPCRewriter("http://myserver.local:43211", "")

	rw.RewriteOutgoing([]byte(`{"command":["observe_property",46,"path"],"request_id":1}`))

	streamURL, err := BuildStreamURL("http://myserver.local:43211", "", testPath)
	if err != nil {
		t.Fatalf("BuildStreamURL: %v", err)
	}

	// No "name" field this time - only "id". RewriteIncoming must fall
	// back to the table learned from the observe_property command above.
	event := `{"event":"property-change","id":46,"data":` + jsonStr(streamURL) + `}`
	rewritten := rw.RewriteIncoming([]byte(event))
	assertDataField(t, rewritten, testPath)
}

func assertDataField(t *testing.T, line []byte, want string) {
	t.Helper()
	var obj struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(line, &obj); err != nil {
		t.Fatalf("rewritten line is not valid JSON: %v (%s)", err, line)
	}
	if obj.Data != want {
		t.Errorf("data = %q, want %q", obj.Data, want)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
