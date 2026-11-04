package dnsprov

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"
)

// MaxErrorLen keeps a provider's error (often a whole response body) readable
// in the panel and small in the database.
const MaxErrorLen = 500

// Scrub renders a provider error with every credential value removed. Provider
// packages echo request URLs and response bodies into their errors, and those
// can carry the token verbatim, URL-encoded or JSON-escaped.
func Scrub(err error, creds map[string]string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, v := range creds {
		if v == "" {
			continue
		}
		for _, form := range append([]string{v, url.QueryEscape(v), url.PathEscape(v)}, jsonForms(v)...) {
			msg = strings.ReplaceAll(msg, form, "[redacted]")
		}
	}
	return msg
}

// jsonForms is v as it appears inside a JSON string, with and without Go's
// HTML escaping (a provider's encoder may do either).
func jsonForms(v string) []string {
	var out []string
	for _, html := range []bool{true, false} {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if enc.Encode(v) != nil {
			continue
		}
		// Encode writes "<escaped>"\n.
		out = append(out, strings.TrimSuffix(strings.TrimSpace(b.String()), `"`)[1:])
	}
	return out
}

// Clip cuts msg to MaxErrorLen bytes on a rune boundary: Postgres refuses a
// TEXT value that ends in half a UTF-8 sequence.
func Clip(msg string) string {
	if len(msg) <= MaxErrorLen {
		return msg
	}
	i := MaxErrorLen
	for i > 0 && !utf8.RuneStart(msg[i]) {
		i--
	}
	return msg[:i] + "..."
}
