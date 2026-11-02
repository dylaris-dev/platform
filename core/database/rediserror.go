package database

import (
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisFailure is why a Redis call failed, in the terms an operator has to act
// on. The distinction that matters is not how severe the failure is - Core
// cannot work without Redis in any of these cases - but whether it can clear on
// its own. An unreachable Redis comes back by itself; a rejected credential or
// a missing ACL permission never does, and waiting for it only delays the fix.
//
// Before this existed, every Redis failure was one undifferentiated condition:
// the health view reported "Connection failed" even when the connection was
// fine and the server had answered NOPERM, and /healthz returned 503 for all of
// them, which asks Swarm to restart a container over a misconfigured ACL that a
// restart cannot repair.
type RedisFailure int

const (
	// RedisOK means the call succeeded.
	RedisOK RedisFailure = iota
	// RedisUnreachable means Core could not talk to the server at all: refused,
	// timed out, DNS failure, connection reset. Transient by nature.
	RedisUnreachable
	// RedisAuthFailed means the server rejected the credentials (WRONGPASS or
	// NOAUTH). Needs an operator.
	RedisAuthFailed
	// RedisPermissionDenied means the credentials were accepted but the ACL user
	// is not allowed to run the command (NOPERM). Needs an operator.
	RedisPermissionDenied
	// RedisServerError means the server answered with some other error. Kept
	// separate from RedisUnreachable so an unexpected reply is never reported as
	// an outage.
	RedisServerError
)

// Redis error prefixes. These are protocol-level replies defined by the server,
// so unlike an OS-level connection error they are stable text and safe to match
// on. Verified against valkey 8: a PING as a user without the +ping permission
// returns "NOPERM User <name> has no permissions to run the 'ping' command",
// wrong credentials return "WRONGPASS invalid username-password pair or user is
// disabled.", and connecting with no credentials to a server that requires them
// returns "NOAUTH Authentication required.".
const (
	redisPrefixNoPerm    = "NOPERM"
	redisPrefixWrongPass = "WRONGPASS"
	redisPrefixNoAuth    = "NOAUTH"
)

// ClassifyRedisError sorts a Redis error into the class an operator can act on.
//
// The split between "the server answered" and "we never reached the server" is
// taken from the error's TYPE, not its text: go-redis returns errors satisfying
// redis.Error for server replies and plain transport errors otherwise. That
// matters, because a connection error's text is produced by the OS and is
// LOCALIZED - on a German Windows host a refused dial reads "Es konnte keine
// Verbindung hergestellt werden", so the substring matching used for Postgres
// in handlers/auth/auth.go would silently classify it as unknown there. Only the
// server's own reply is matched as text.
//
// Anything the server returns that is not one of the three known auth/ACL
// prefixes is RedisServerError rather than RedisUnreachable: the server is
// demonstrably reachable, and reporting an outage would be wrong.
func ClassifyRedisError(err error) RedisFailure {
	if err == nil {
		return RedisOK
	}

	var serverErr redis.Error
	if !errors.As(err, &serverErr) {
		return RedisUnreachable
	}

	msg := serverErr.Error()
	switch {
	case strings.HasPrefix(msg, redisPrefixNoPerm):
		return RedisPermissionDenied
	case strings.HasPrefix(msg, redisPrefixWrongPass), strings.HasPrefix(msg, redisPrefixNoAuth):
		return RedisAuthFailed
	default:
		return RedisServerError
	}
}

// Slug is the stable machine-readable name for a class. The panel keys its
// guidance text on this, so these strings are part of the API and renaming one
// silently drops that guidance back to the generic case.
func (f RedisFailure) Slug() string {
	switch f {
	case RedisOK:
		return "ok"
	case RedisUnreachable:
		return "unreachable"
	case RedisAuthFailed:
		return "auth"
	case RedisPermissionDenied:
		return "permission"
	default:
		return "server_error"
	}
}

// NeedsOperator reports whether this class will still be here after any amount
// of waiting. It is what decides whether a failure should be allowed to gate a
// container healthcheck: restarting Core cannot fix a credential or an ACL rule.
func (f RedisFailure) NeedsOperator() bool {
	return f == RedisAuthFailed || f == RedisPermissionDenied
}

// Summary is the short line shown beside the component in the health view. It
// replaces a single hardcoded "Connection failed" that was simply untrue for
// two of these classes.
//
// The auth wording deliberately lists three causes. Redis answers WRONGPASS for
// a wrong password, an unknown username AND a disabled user, and does not say
// which - verified against valkey 8 - so naming only one would send an operator
// looking in the wrong place.
func (f RedisFailure) Summary() string {
	switch f {
	case RedisOK:
		return "Connection alive"
	case RedisUnreachable:
		return "Cannot reach the server"
	case RedisAuthFailed:
		return "Credentials rejected"
	case RedisPermissionDenied:
		return "Permission denied by ACL"
	default:
		return "Server returned an error"
	}
}
