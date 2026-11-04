package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"dylaris-core/services/dnsprov"
)

// defaultMinecraftPort is the port a client uses when no SRV record says otherwise.
const defaultMinecraftPort = 25565

// Domain states. There is deliberately no red: a name that is wrong is yellow.
const (
	DomainGrey   = "grey"
	DomainYellow = "yellow"
	DomainGreen  = "green"
)

// ServerDomainResolver is the DNS surface the evaluator needs, as an interface so
// tests never touch the network. Hosts are plain names without a trailing dot;
// SRV hosts are the full "_minecraft._tcp.<fqdn>".
type ServerDomainResolver interface {
	LookupA(ctx context.Context, host string) ([]netip.Addr, error)
	LookupAAAA(ctx context.Context, host string) ([]netip.Addr, error)
	LookupSRV(ctx context.Context, host string) ([]*net.SRV, error)
}

type netServerDomainResolver struct{ r *net.Resolver }

// NewNetServerDomainResolver returns the production resolver. It uses the pure-Go
// resolver and always queries ABSOLUTE names: with a relative name the system
// search list applies, and a search-domain wildcard once turned a non-existent
// name into a public address (see custom_domain_proof.go).
func NewNetServerDomainResolver() ServerDomainResolver {
	return &netServerDomainResolver{r: &net.Resolver{PreferGo: true}}
}

const domainLookupTimeout = 5 * time.Second

func absoluteName(h string) string { return strings.TrimSuffix(h, ".") + "." }

func (n *netServerDomainResolver) LookupA(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()
	return n.r.LookupNetIP(ctx, "ip4", absoluteName(host))
}

func (n *netServerDomainResolver) LookupAAAA(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()
	return n.r.LookupNetIP(ctx, "ip6", absoluteName(host))
}

func (n *netServerDomainResolver) LookupSRV(ctx context.Context, host string) ([]*net.SRV, error) {
	ctx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()
	// Empty service and proto make Go query the name exactly as given.
	_, srvs, err := n.r.LookupSRV(ctx, "", "", absoluteName(host))
	return srvs, err
}

// ExpectedRecords is what Core writes for one managed domain: an A record, plus a
// Minecraft SRV when the server is not on the default port. Names are relative to
// the zone.
func ExpectedRecords(label, zone string, nodeIP netip.Addr, port uint16) []dnsprov.Record {
	recs := []dnsprov.Record{{Name: label, Type: "A", Value: nodeIP.Unmap().String()}}
	if port != defaultMinecraftPort {
		recs = append(recs, dnsprov.Record{
			Name: "_minecraft._tcp." + label, Type: "SRV",
			Value: label + "." + zone, Port: port,
		})
	}
	return recs
}

// DomainStatus is what DNS actually answers, judged against the node.
type DomainStatus struct {
	State    string
	Problems []string
}

// lookupFailed separates a failed lookup (timeout, SERVFAIL, temporary) from a
// definitive negative answer (NXDOMAIN or no data for the type), which is a
// normal "no record". Only the former says nothing about the zone.
func lookupFailed(err error) bool {
	var de *net.DNSError
	return err != nil && !(errors.As(err, &de) && de.IsNotFound)
}

// EvaluateDomain judges the live DNS answers for fqdn, never the records Core
// believes it wrote. Any FAILED lookup makes the result grey: a status built on a
// guess could be a green that is not true.
func EvaluateDomain(ctx context.Context, r ServerDomainResolver, fqdn string, nodeIP netip.Addr, port uint16) DomainStatus {
	fqdn = strings.TrimSuffix(strings.ToLower(fqdn), ".")
	if !nodeIP.IsValid() {
		return DomainStatus{DomainGrey, []string{"the node has not reported a public IP yet"}}
	}
	failed := func(typ string) DomainStatus {
		return DomainStatus{DomainGrey, []string{fmt.Sprintf("DNS lookup failed (%s), checking again shortly", typ)}}
	}
	as, err := r.LookupA(ctx, fqdn)
	if lookupFailed(err) {
		return failed("A")
	}
	if len(as) == 0 {
		return DomainStatus{DomainGrey, []string{"does not resolve yet"}}
	}
	var problems []string

	// A: fine when the node is among the answers; otherwise name what it points to.
	hasNode := false
	var wrong []string
	for _, a := range as {
		if a.Unmap() == nodeIP.Unmap() {
			hasNode = true
		} else {
			wrong = append(wrong, a.String())
		}
	}
	switch {
	case !hasNode:
		problems = append(problems, fmt.Sprintf("points to %s, should be %s", strings.Join(wrong, ", "), nodeIP))
	case len(wrong) > 0:
		// Players can be handed the other address.
		problems = append(problems, fmt.Sprintf("also points to %s, should only be %s", strings.Join(wrong, ", "), nodeIP))
	}

	// SRV
	srvs, err := r.LookupSRV(ctx, "_minecraft._tcp."+fqdn)
	if lookupFailed(err) {
		return failed("SRV")
	}
	if len(srvs) == 0 {
		if port != defaultMinecraftPort {
			problems = append(problems, fmt.Sprintf("Players would have to type :%d - add an SRV record _minecraft._tcp.%s pointing to %s port %d", port, fqdn, fqdn, port))
		}
	} else {
		sp, failedLookup := srvProblems(ctx, r, srvs, port, nodeIP)
		if failedLookup {
			return failed("SRV target A")
		}
		problems = append(problems, sp...)
	}

	// AAAA: Core never writes one, so any answer sends IPv6 players elsewhere.
	v6, err := r.LookupAAAA(ctx, fqdn)
	if lookupFailed(err) {
		return failed("AAAA")
	}
	if len(v6) > 0 {
		s := make([]string, len(v6))
		for i, a := range v6 {
			s[i] = a.String()
		}
		problems = append(problems, fmt.Sprintf("IPv6 players reach a different host (AAAA %s)", strings.Join(s, ", ")))
	}

	if len(problems) > 0 {
		return DomainStatus{DomainYellow, problems}
	}
	return DomainStatus{State: DomainGreen}
}

// srvProblems judges EVERY SRV record: one good record does not excuse another
// that sends players elsewhere (clients may pick any record by priority/weight).
// A record is good when its port is the server port and its target resolves to
// exactly the node IP. Records are sorted so the output order is stable.
func srvProblems(ctx context.Context, r ServerDomainResolver, srvs []*net.SRV, port uint16, nodeIP netip.Addr) (problems []string, lookupErr bool) {
	srvs = append([]*net.SRV(nil), srvs...)
	sort.Slice(srvs, func(i, j int) bool {
		if srvs[i].Target != srvs[j].Target {
			return srvs[i].Target < srvs[j].Target
		}
		return srvs[i].Port < srvs[j].Port
	})
	anyPort := false
	for _, s := range srvs {
		anyPort = anyPort || s.Port == port
	}
	if !anyPort {
		return []string{fmt.Sprintf("SRV points to port %d, should be %d", srvs[0].Port, port)}, false
	}
	type rec struct {
		label, detail string // detail "" means the record is good
	}
	recs := make([]rec, len(srvs))
	anyGood := false
	for i, s := range srvs {
		target := strings.TrimSuffix(strings.ToLower(s.Target), ".")
		recs[i].label = fmt.Sprintf("%s:%d", target, s.Port)
		if s.Port != port {
			recs[i].detail = "SRV also points to " + recs[i].label
			continue
		}
		as, err := r.LookupA(ctx, target)
		if lookupFailed(err) {
			return nil, true
		}
		var extra []string
		hasNode := false
		for _, a := range as {
			if a.Unmap() == nodeIP.Unmap() {
				hasNode = true
			} else {
				extra = append(extra, a.String())
			}
		}
		switch {
		case !hasNode:
			recs[i].detail = fmt.Sprintf("SRV target %s does not resolve to %s", target, nodeIP.Unmap())
		case len(extra) > 0:
			recs[i].detail = fmt.Sprintf("SRV target %s also points to %s, should only be %s", target, strings.Join(extra, ", "), nodeIP.Unmap())
		default:
			anyGood = true
		}
	}
	for _, rc := range recs {
		if rc.detail == "" {
			continue
		}
		// Next to a good record an offender is an extra destination, not the
		// primary problem.
		if anyGood && !strings.HasPrefix(rc.detail, "SRV also") {
			rc.detail = "SRV also points to " + rc.label
		}
		problems = append(problems, rc.detail)
	}
	return problems, false
}
