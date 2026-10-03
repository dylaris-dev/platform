package services

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"strings"
)

// Custom-domain ownership proof.
//
// A tenant may point their own domain at the platform, but only after showing
// they control it. The proof is a TXT record carrying a token minted for that
// one (account, domain) claim.
//
// It used to be "the domain resolves to us", and that proves nothing about
// WHICH tenant: a CNAME a former customer left behind, or any name under a
// wildcard pointed at us, passed for whoever entered it first - and their
// players then landed on that tenant's server.

// DomainResolver is the DNS surface the proof needs, as an interface so tests
// can drive it without touching the network.
//
// Injectable on purpose, and not only for convenience: a resolver that answers
// NXDOMAIN with a search-domain wildcard (which is how a real lookup burned us
// once - a name that did not exist came back with a public address) must not be
// able to turn "not configured" into a pass. Every check below compares the
// answer against a value only we know or control, so a bogus answer fails
// closed rather than sneaking through.
type DomainResolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupTXT(ctx context.Context, host string) ([]string, error)
}

// netResolver is the production DomainResolver.
type netResolver struct{ r *net.Resolver }

// NewNetResolver returns the system-resolver implementation.
func NewNetResolver() DomainResolver { return &netResolver{r: net.DefaultResolver} }

func (n *netResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return n.r.LookupCNAME(ctx, host)
}
func (n *netResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return n.r.LookupHost(ctx, host)
}
func (n *netResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	return n.r.LookupTXT(ctx, host)
}

// TXTVerifyPrefix is the label a tenant adds to prove ownership the strict way.
const TXTVerifyPrefix = "_dylaris-verify."

// normaliseHost lowercases and drops the trailing dot DNS answers carry.
func normaliseHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// NewTXTToken mints a claim's ownership token.
func NewTXTToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dylaris-verify=" + hex.EncodeToString(b), nil
}

// CheckTXTToken reports whether _dylaris-verify.<domain> carries token.
//
// A TXT record at a dylaris-specific label is a value only the zone's owner can
// publish, and only this claim's token satisfies it.
//
// Compared in constant time. The token is a credential, and a timing signal is
// free to exploit here because the attacker controls how often they ask.
func CheckTXTToken(ctx context.Context, res DomainResolver, domain, token string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	records, err := res.LookupTXT(ctx, TXTVerifyPrefix+normaliseHost(domain))
	if err != nil {
		return false
	}
	for _, r := range records {
		r = strings.TrimSpace(r)
		if len(r) == len(token) && subtle.ConstantTimeCompare([]byte(r), []byte(token)) == 1 {
			return true
		}
	}
	return false
}
