package handlers

import "net/http"

// hstsValue keeps a browser on HTTPS for a year once it has seen the panel
// over HTTPS. The plain-HTTP port only redirects, which an attacker on the
// path can strip from the very first request; with this the browser never
// makes that request again. No includeSubDomains: other hosts under the same
// domain are not this program's to promise for.
const hstsValue = "max-age=31536000"

// SecurityHeaders sets the headers every panel and API response carries. Tenant
// tab hosts never reach it - they are routed off before this handler - since
// a tenant's content sets its own.
//
// nosniff was on the panel HTML and a few downloads only; HSTS was nowhere.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if requestIsHTTPS(r) {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}
