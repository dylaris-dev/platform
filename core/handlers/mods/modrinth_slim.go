package mods

import (
	"bytes"
	"encoding/json"
)

// Trimming a Modrinth version payload before it is cached or served.
//
// The obvious idea is to keep only the newest build per game version, since a
// popular mod carries up to 40 builds for a single one (measured: Sodium has 40
// for 1.21.1, Fabric API 34). That idea is wrong here, for two reasons:
//
//   - the version pickers exist precisely to install something other than the
//     newest. The project detail column fetches loader-only, unfiltered by game
//     version, and says so in a comment, because an operator whose server broke
//     on the current build goes back one.
//   - it would not even be the big win. Measured per field on Sodium's list:
//
//     changelog  291,471 B  66.2%
//     files      103,395 B  23.5%
//     everything else                  10.3%
//
// The changelog is two thirds of the payload and NOTHING renders it: the panel
// declares the field and never reads it. Dropping it takes more out than
// dropping 80% of the builds would, and costs no capability at all.
//
// Stripped on the way through rather than only on the way into the cache, so a
// cache hit and a cache miss return the same document. A field that appears on
// the first load and vanishes on the second is a worse bug than the bytes.

// stripVersionChangelogs removes the "changelog" field from a Modrinth version
// object, or from every element of an array of them.
//
// It works on json.RawMessage so every field it keeps is preserved byte for
// byte. Round-tripping through interface{} would re-encode numbers through
// float64 and reorder keys, which is a lot of risk for a field this trims by
// name anyway.
//
// Input it cannot parse is returned unchanged: this is a size optimisation, and
// failing it must never turn into failing the request.
func stripVersionChangelogs(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	switch trimmed[0] {
	case '[':
		var list []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return body
		}
		for _, obj := range list {
			delete(obj, "changelog")
		}
		out, err := json.Marshal(list)
		if err != nil {
			return body
		}
		return out
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return body
		}
		if _, ok := obj["changelog"]; !ok {
			return body
		}
		delete(obj, "changelog")
		out, err := json.Marshal(obj)
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}
