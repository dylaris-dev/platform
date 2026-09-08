package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq"
)

// What every "Test connection" button in the panel reports back.
//
// The point of this file is one distinction the old test buttons could not
// make: whether the host answered at all. "Connection failed" covers a typo in
// the hostname, a firewall dropping the packet, a wrong password and a database
// that does not exist - four problems fixed in four different places, told
// apart by no part of that message. An operator reading it can only guess, and
// the usual guess is that the credentials are wrong, which is the one thing
// they can retype without learning anything.
//
// So a test happens in two steps. Open a TCP connection to the host and port
// first: if nothing answers, the credentials were never in play and saying
// anything about them is noise. Only once something answers does the real
// attempt run, and then a rejection genuinely IS about what was rejected.

// ConnStage says how far the attempt got. The panel colours and words its
// message from this, so the three values are a contract, not a log line.
const (
	// StageUnreachable - nothing answered on that host and port. Wrong host,
	// wrong port, service not running, or a firewall that drops rather than
	// refuses. Credentials are irrelevant and must not be mentioned.
	StageUnreachable = "unreachable"
	// StageRejected - something answered and then said no. Wrong password, no
	// such database, TLS refused, permission denied. The address is right.
	StageRejected = "rejected"
	// StageOK - answered, accepted, and the probe query ran.
	StageOK = "ok"
)

// ConnCheck is the result of a two-step test.
type ConnCheck struct {
	OK      bool   `json:"ok"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

// Unreachable builds the "nothing answered" result.
func Unreachable(msg string) ConnCheck {
	return ConnCheck{Stage: StageUnreachable, Message: msg}
}

// Rejected builds the "answered, then refused" result.
func Rejected(msg string) ConnCheck {
	return ConnCheck{Stage: StageRejected, Message: msg}
}

// dialBudget bounds the reachability step.
//
// Short on purpose, and shorter than the connect that follows it. This step
// answers one question - is anything listening - and every millisecond it
// spends beyond that is a panel that looks hung. A host on the same overlay
// answers in single-digit milliseconds; one that needs longer than this is
// telling us something either way.
const dialBudget = 3 * time.Second

// Reachable opens and immediately closes a TCP connection.
//
// It reports the reason in the operator's terms rather than the resolver's.
// "no such host" is a name that does not resolve, which on a Docker network
// usually means the service name is wrong or the container is not on the same
// network - a different fix from a refused port, and the two must not arrive
// as the same sentence.
func Reachable(ctx context.Context, host, port string) ConnCheck {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		return Unreachable("No host given.")
	}
	if port == "" {
		return Unreachable("No port given.")
	}

	d := net.Dialer{Timeout: dialBudget}
	dctx, cancel := context.WithTimeout(ctx, dialBudget)
	defer cancel()

	conn, err := d.DialContext(dctx, "tcp", net.JoinHostPort(host, port))
	if err == nil {
		conn.Close()
		return ConnCheck{OK: true, Stage: StageOK}
	}

	where := host + ":" + port
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return Unreachable(fmt.Sprintf(
			"The name %q does not resolve. On a Docker network that is usually a service name that does "+
				"not exist, or a container that is not attached to the same network as Core.", host))
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return Unreachable(fmt.Sprintf(
			"No answer from %s within %d seconds. A dropped packet rather than a refusal - typically a "+
				"firewall, a security group, or a host that is not up.", where, int(dialBudget.Seconds())))
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		return Unreachable(fmt.Sprintf(
			"%s refused the connection. The host is up and reachable, but nothing is listening on that "+
				"port - check the port, and that the service is running.", where))
	}
	return Unreachable(fmt.Sprintf("Could not reach %s: %v", where, err))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// DescribePostgresError turns a driver error into the sentence that names the
// fix.
//
// The SQLSTATE codes are the whole reason this exists: PostgreSQL already
// distinguishes "wrong password" from "no such database" from "that role may
// not connect", and lib/pq carries the code through. Passing err.Error()
// straight to the panel throws that away and hands the operator a driver
// message that mentions neither what was wrong nor where.
func DescribePostgresError(err error) string {
	if err == nil {
		return ""
	}
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			// 28P01 invalid_password, 28000 invalid_authorization_specification.
			return "The database answered and rejected the login: wrong user or password. " +
				"Check pg_hba.conf too - a role can be correct and still not be allowed in from this host."
		case "3D000":
			// invalid_catalog_name. The single most common one here, because a
			// separate database is something an operator has to create first.
			return "Connected to the server, but that database does not exist on it. " +
				"Create it first: CREATE DATABASE <name> OWNER <user>;"
		case "42501":
			return "Connected and authenticated, but that user is not allowed to do this here. " +
				"It needs rights on the database, not only a login."
		case "53300":
			return "The server is up but has no connection slots left (max_connections). " +
				"Nothing is wrong with these settings; the server is full."
		case "57P03":
			return "The server is starting up and not accepting connections yet. Try again in a moment."
		}
		if pgErr.Message != "" {
			return "The database refused the connection: " + pgErr.Message
		}
	}

	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "ssl is not enabled") || strings.Contains(low, "server does not support ssl"):
		return "The server does not offer TLS, but this target asks for it. Set SSL mode to disable, " +
			"which is the normal answer for a database reached over a private network."
	case strings.Contains(low, "certificate"):
		return "TLS was refused: " + msg + ". A verify-ca or verify-full mode needs a certificate this " +
			"client trusts; require encrypts without checking who is on the other end."
	case strings.Contains(low, "context deadline exceeded") || strings.Contains(low, "timeout"):
		return "The host accepted the connection and then stopped answering. That is not a credential " +
			"problem: something is listening on that port that is not this database, or the server is stuck."
	}
	return msg
}

// HostPortFromEndpoint pulls the host and port out of an S3-style endpoint.
//
// Endpoints arrive in every shape an operator can type: with a scheme, without
// one, with a trailing slash, with an explicit port or none. Without a port
// there is nothing to dial, so the scheme decides it - and http:// on an
// endpoint that is really TLS would fail the reachability step for the wrong
// reason, which is why the default follows the scheme rather than being fixed.
func HostPortFromEndpoint(endpoint string) (host, port string, ok bool) {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return "", "", false
	}
	if !strings.Contains(e, "://") {
		e = "https://" + e
	}
	u, err := url.Parse(e)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return host, port, host != ""
}

// HostPortFromPostgresDSN pulls host and port out of a connection string.
//
// Both forms an operator can type, because both are accepted elsewhere here:
// the URL form (postgres://user:pw@host:5432/db) that the ticket screen asks
// for, and the keyword form (host=... port=...) that DBConnParams renders. A
// DSN whose host cannot be found is not an error worth raising - it only means
// the reachability step is skipped and the driver gets to speak first.
func HostPortFromPostgresDSN(dsn string) (host, port string, ok bool) {
	d := strings.TrimSpace(dsn)
	if d == "" {
		return "", "", false
	}
	if strings.HasPrefix(d, "postgres://") || strings.HasPrefix(d, "postgresql://") {
		u, err := url.Parse(d)
		if err != nil || u.Hostname() == "" {
			return "", "", false
		}
		port = u.Port()
		if port == "" {
			port = "5432"
		}
		return u.Hostname(), port, true
	}
	for _, field := range strings.Fields(d) {
		k, v, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch strings.ToLower(k) {
		case "host":
			host = strings.Trim(v, "'\"")
		case "port":
			port = strings.Trim(v, "'\"")
		}
	}
	if host == "" {
		return "", "", false
	}
	if port == "" {
		port = "5432"
	}
	return host, port, true
}

// ClassifyStorageFailure reads a failed storage probe and says which of the two
// steps it failed at.
//
// It works on the message rather than the error because the probe writes, reads
// back and deletes through an interface that flattens everything to text on the
// way out. Substring matching is not elegant; the alternative was threading a
// typed error through four backends to answer one question, and every string
// matched here is one an S3 implementation is contractually required to emit.
func ClassifyStorageFailure(msg string) ConnCheck {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "no such host"):
		return Unreachable("The endpoint's hostname does not resolve. Check it for a typo, and that it " +
			"includes the region if the provider needs one.")
	case strings.Contains(low, "connection refused"):
		return Unreachable("The endpoint refused the connection: reachable, but nothing is listening on " +
			"that port. Check the port and the scheme (http versus https).")
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded") ||
		strings.Contains(low, "i/o timeout"):
		return Unreachable("No answer from the endpoint. A dropped packet rather than a refusal - " +
			"typically a firewall or an endpoint that is not up.")
	case strings.Contains(low, "certificate") || strings.Contains(low, "tls"):
		return Unreachable("The TLS handshake failed: " + msg + ". Check whether the endpoint really " +
			"speaks https, and whether its certificate is one this server trusts.")
	case strings.Contains(low, "invalidaccesskeyid") || strings.Contains(low, "signaturedoesnotmatch") ||
		strings.Contains(low, "invalid access key") || strings.Contains(low, "403"):
		return Rejected("The endpoint answered and rejected the credentials: the access key or the " +
			"secret is wrong for this endpoint.")
	case strings.Contains(low, "nosuchbucket") || strings.Contains(low, "bucket does not exist"):
		return Rejected("The endpoint answered, the credentials were accepted, but that bucket does not " +
			"exist. Create it, or correct the name.")
	case strings.Contains(low, "accessdenied") || strings.Contains(low, "access denied"):
		return Rejected("The endpoint answered and the credentials are valid, but this key may not do " +
			"that here. It needs read, write and delete on this bucket, not only read.")
	}
	return Rejected(msg)
}
