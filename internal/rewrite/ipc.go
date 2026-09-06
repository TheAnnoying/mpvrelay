package rewrite

import (
	"encoding/json"
	"path/filepath"
)

// commandRequest mirrors mpv's IPC command wire format:
// {"command":[...],"request_id":N}.
type commandRequest struct {
	Command   []interface{} `json:"command"`
	RequestID uint          `json:"request_id"`
}

type propertyEvent struct {
	Event string          `json:"event"`
	Name  string          `json:"name"`
	Data  json.RawMessage `json:"data"`
}

// IPCRewriter is only used stub-side (cmd/mpv), the side that knows the
// server's base URL and password.
type IPCRewriter struct {
	serverBaseURL  string
	serverPassword string
}

func NewIPCRewriter(serverBaseURL, serverPassword string) *IPCRewriter {
	return &IPCRewriter{
		serverBaseURL:  serverBaseURL,
		serverPassword: serverPassword,
	}
}

// RewriteOutgoing rewrites a "loadfile" command's real path into a
// mediastream URL; every other line passes through unmodified, including
// a loadfile whose URL fails to build (fails open, so mpv still gets the
// command and errors visibly instead of the line silently vanishing).
func (rw *IPCRewriter) RewriteOutgoing(line []byte) []byte {
	var req commandRequest
	if err := json.Unmarshal(line, &req); err != nil || len(req.Command) == 0 {
		return line
	}

	name, _ := req.Command[0].(string)
	switch name {
	case "loadfile":
		if len(req.Command) < 2 {
			return line
		}
		realPath, ok := req.Command[1].(string)
		if !ok || realPath == "" {
			return line
		}
		streamURL, err := BuildStreamURL(rw.serverBaseURL, rw.serverPassword, realPath)
		if err != nil {
			return line
		}
		req.Command[1] = streamURL
		out, err := json.Marshal(req)
		if err != nil {
			return line
		}
		return out

	default:
		return line
	}
}

// RewriteIncoming rewrites "path"/"filename" property-change events
// reporting one of our mediastream URLs back to the real path; every
// other line (including a null "path" before anything is loaded) passes
// through unmodified.
func (rw *IPCRewriter) RewriteIncoming(line []byte) []byte {
	var ev propertyEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Event != "property-change" {
		return line
	}

	if ev.Name != "path" && ev.Name != "filename" {
		return line
	}

	var raw string
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		return line // data isn't a string (e.g. null before a file is loaded)
	}

	realPath, ok := ExtractPathFromStreamURL(raw)
	if !ok {
		return line
	}

	var replacement string
	if ev.Name == "filename" {
		replacement = filepath.Base(realPath)
	} else {
		replacement = realPath
	}

	return replaceJSONField(line, "data", replacement)
}

// replaceJSONField replaces one top-level field's value, leaving every
// other field (including ones this package doesn't know about) untouched.
func replaceJSONField(line []byte, field string, value string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return line
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return line
	}
	obj[field] = encoded
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	return out
}
