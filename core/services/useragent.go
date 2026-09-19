package services

import "sync/atomic"

// Every request Core makes to a third party (Modrinth, Technic, a modpack
// host) identifies itself with ONE string: Modrinth's API docs threaten to
// block generic agents, and the Technic API sees us as one caller for every
// customer. There used to be five hand-written variants, one without a contact.

var userAgentRelease atomic.Value // string

// SetUserAgentRelease records the release this Core runs, once at boot. Empty
// (a development build) reads as "dev".
func SetUserAgentRelease(v string) { userAgentRelease.Store(v) }

// DylarisUserAgent is the User-Agent for outbound calls.
func DylarisUserAgent() string {
	v, _ := userAgentRelease.Load().(string)
	if v == "" {
		v = "dev"
	}
	return "Dylaris/" + v + " (+https://dylaris.com)"
}
