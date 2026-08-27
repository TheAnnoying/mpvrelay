package rewrite

import (
	"encoding/json"
	"path/filepath"
	"sync"
)

// commandRequest mirrors internal/mediaplayers/mpvipc/mpvipc.go's
// commandRequest wire format exactly: {"command":[...],"request_id":N}.
type commandRequest struct {
	Command   []interface{} `json:"command"`
	RequestID uint          `json:"request_id"`
}

// propertyEvent covers the fields of a mpv JSON-IPC event that this
// package cares about. mpv's actual property-change events include the
// property "name" alongside its "id" (see mpv's JSON IPC documentation);
// Seanime's own mpvipc.Event struct doesn't bother decoding "name" since
// it doesn't need it (it already knows, at the call site, which numeric
// ID means what — it assigned them). This tool doesn't have that luxury,
// so it decodes "name" directly when present. observedNames (populated
// by watching Seanime's own observe_property commands as they pass
// through, in RewriteOutgoing) is kept purely as a fallback for the case
// where "name" is ever absent from the wire event — this hasn't been
// observed, but costs almost nothing to guard against.
type propertyEvent struct {
	Event string          `json:"event"`
	ID    uint            `json:"id"`
	Name  string          `json:"name"`
	Data  json.RawMessage `json:"data"`
}

// IPCRewriter holds the (small) session state needed to interpret IPC
// traffic for one playback session: the property-observation ID->name
// table learned from watching Seanime's own observe_property calls go
// by. It has nothing to do with which file is currently loaded — see the
// package doc for why that isn't tracked as state at all.
//
// An IPCRewriter is only ever used from the stub side (cmd/mpv): it's
// the side that knows the server's base URL, password and the real
// absolute path Seanime asked to load. The agent (cmd/mpv-agent) never
// looks inside an IPC line.
type IPCRewriter struct {
	serverBaseURL  string
	serverPassword string

	mu            sync.Mutex
	observedNames map[uint]string // property ID -> name, learned from observe_property commands
}

func NewIPCRewriter(serverBaseURL, serverPassword string) *IPCRewriter {
	return &IPCRewriter{
		serverBaseURL:  serverBaseURL,
		serverPassword: serverPassword,
		observedNames:  make(map[uint]string),
	}
}

// RewriteOutgoing rewrites one line flowing from Seanime toward the real
// mpv (i.e. mpv commands). It:
//   - rewrites "loadfile <realAbsPath> <mode>" commands' path argument
//     into a mediastream URL, so the client's mpv can actually fetch it.
//   - records "observe_property <id> <name>" commands' id/name mapping,
//     for RewriteIncoming's id-based fallback.
//   - passes every other line through completely unmodified, including
//     when it fails to build a stream URL for a loadfile (fails open:
//     the command still reaches mpv, just with the un-rewritten local
//     path, which will fail to open on the client with a visible error
//     instead of silently vanishing).
func (rw *IPCRewriter) RewriteOutgoing(line []byte) []byte {
	var req commandRequest
	if err := json.Unmarshal(line, &req); err != nil || len(req.Command) == 0 {
		return line
	}

	name, _ := req.Command[0].(string)
	switch name {
	case "observe_property":
		if len(req.Command) >= 3 {
			id, idOK := toUint(req.Command[1])
			propName, nameOK := req.Command[2].(string)
			if idOK && nameOK {
				rw.mu.Lock()
				rw.observedNames[id] = propName
				rw.mu.Unlock()
			}
		}
		return line

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

// RewriteIncoming rewrites one line flowing from the real mpv back
// toward Seanime (i.e. mpv events/results). It only ever touches
// property-change events for "path" or "filename" whose data is one of
// this tool's own mediastream URLs; everything else - including a
// property-change for "path"/"filename" whose data ISN'T one of our
// URLs (e.g. mpv hasn't loaded anything yet and reports null) - passes
// through byte-for-byte unmodified.
func (rw *IPCRewriter) RewriteIncoming(line []byte) []byte {
	var ev propertyEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Event != "property-change" {
		return line
	}

	name := ev.Name
	if name == "" {
		rw.mu.Lock()
		name = rw.observedNames[ev.ID]
		rw.mu.Unlock()
	}
	if name != "path" && name != "filename" {
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
	if name == "filename" {
		replacement = filepath.Base(realPath)
	} else {
		replacement = realPath
	}

	return replaceJSONField(line, "data", replacement)
}

// replaceJSONField re-encodes value (a Go value, here always a string)
// as the given top-level field of the JSON object in line, leaving every
// other field and its original value untouched. It operates generically
// (map[string]json.RawMessage) rather than through a fixed struct so
// that fields this package doesn't know about (mpv adds new ones over
// time) are preserved rather than silently dropped.
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

func toUint(v interface{}) (uint, bool) {
	f, ok := v.(float64) // encoding/json decodes all JSON numbers as float64 into interface{}
	if !ok || f < 0 {
		return 0, false
	}
	return uint(f), true
}
