// Package dnsprov adapts the libdns provider packages to the small interface
// the direct-mode domain reconciler needs: A and Minecraft SRV records,
// addressed relative to a zone.
package dnsprov

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/libdns/libdns"
)

// recordTTL is deliberately short: the whole point of the updater is following a
// node's IP, and a long TTL would keep players on the old address after a move.
const recordTTL = 60 * time.Second

// srvWeight is the value the spec fixes; priority stays 0 because there is only
// ever one target per name.
const srvWeight = 5

// ErrNoZoneListing is returned by ListZones when the provider package has no
// ZoneLister. The caller falls back to asking the user for the zone.
var ErrNoZoneListing = errors.New("provider does not support listing zones")

// Record is one DNS record Core manages. Name is relative to the zone.
type Record struct {
	Name       string
	Type       string // "A" or "SRV"; GetRecords also reports "CNAME" (name only, no Value)
	Value      string // A: the IPv4 address. SRV: the target FQDN, without trailing dot.
	Port       uint16 // SRV only
	ProviderID string // whatever the provider package reports, empty if it reports nothing
}

// Provider is what the reconciler talks to.
type Provider interface {
	ListZones(ctx context.Context) ([]string, error)
	GetRecords(ctx context.Context, zone string) ([]Record, error)
	SetRecords(ctx context.Context, zone string, recs []Record) ([]Record, error)
	DeleteRecords(ctx context.Context, zone string, recs []Record) error
}

// adapter wraps any libdns provider value. The interfaces are asserted once at
// construction so a package that lacks one fails at New, not on the first sync.
type adapter struct {
	get   libdns.RecordGetter
	set   libdns.RecordSetter
	del   libdns.RecordDeleter
	zones libdns.ZoneLister // nil when the package cannot list zones
}

func newAdapter(p any) (*adapter, error) {
	a := &adapter{}
	var ok bool
	if a.get, ok = p.(libdns.RecordGetter); !ok {
		return nil, fmt.Errorf("provider %T cannot read records", p)
	}
	if a.set, ok = p.(libdns.RecordSetter); !ok {
		return nil, fmt.Errorf("provider %T cannot set records", p)
	}
	if a.del, ok = p.(libdns.RecordDeleter); !ok {
		return nil, fmt.Errorf("provider %T cannot delete records", p)
	}
	a.zones, _ = p.(libdns.ZoneLister)
	return a, nil
}

func (a *adapter) ListZones(ctx context.Context) ([]string, error) {
	if a.zones == nil {
		return nil, ErrNoZoneListing
	}
	zs, err := a.zones.ListZones(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(zs))
	for _, z := range zs {
		out = append(out, strings.ToLower(strings.TrimSuffix(z.Name, ".")))
	}
	return out, nil
}

func (a *adapter) GetRecords(ctx context.Context, zone string) ([]Record, error) {
	rs, err := a.get.GetRecords(ctx, zone)
	if err != nil {
		return nil, err
	}
	return fromLibdns(rs)
}

func (a *adapter) SetRecords(ctx context.Context, zone string, recs []Record) ([]Record, error) {
	in, err := toLibdnsAll(recs, recordTTL)
	if err != nil {
		return nil, err
	}
	out, err := a.set.SetRecords(ctx, zone, in)
	if err != nil {
		return nil, err
	}
	return fromLibdns(out)
}

func (a *adapter) DeleteRecords(ctx context.Context, zone string, recs []Record) error {
	// TTL 0 is libdns' wildcard: delete matches on name, type, TTL and value, and a
	// provider may store another TTL than the 60s we wrote (minimum-TTL providers),
	// which would make an exact match silently delete nothing. An SRV value is one
	// string ("priority weight port target") with no per-field wildcard, so it
	// stays exact; it is the 0/5 we wrote ourselves.
	in, err := toLibdnsAll(recs, 0)
	if err != nil {
		return err
	}
	_, err = a.del.DeleteRecords(ctx, zone, in)
	return err
}

func toLibdnsAll(recs []Record, ttl time.Duration) ([]libdns.Record, error) {
	out := make([]libdns.Record, 0, len(recs))
	for _, r := range recs {
		l, err := toLibdns(r, ttl)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

func toLibdns(r Record, ttl time.Duration) (libdns.Record, error) {
	// libdns requires a name and uses "@" for the zone apex.
	if r.Name == "" {
		r.Name = "@"
	}
	switch r.Type {
	case "A":
		ip, err := netip.ParseAddr(r.Value)
		if err != nil || !ip.Is4() {
			return nil, fmt.Errorf("record %q: %q is not an IPv4 address", r.Name, r.Value)
		}
		return libdns.Address{Name: r.Name, TTL: ttl, IP: ip}, nil
	case "SRV":
		return libdns.SRV{
			Service:   "minecraft",
			Transport: "tcp",
			Name:      r.Name,
			TTL:       ttl,
			Priority:  0,
			Weight:    srvWeight,
			Port:      r.Port,
			// The trailing dot makes the target absolute; without it some providers
			// append the zone name to it.
			Target: strings.TrimSuffix(r.Value, ".") + ".",
		}, nil
	}
	return nil, fmt.Errorf("record %q: unsupported type %q", r.Name, r.Type)
}

// fromLibdns keeps A records, Minecraft SRV records and CNAMEs (name only);
// everything else in the zone is not ours to look at and is dropped. AAAA is
// dropped too: the reconciler never writes one, and DNS answers decide the status.
// An A record, or a _minecraft._tcp SRV, that does not parse is an ERROR rather
// than a skip: dropping it would let the caller believe the name is free and
// create a duplicate. The error names the record, never its content.
func fromLibdns(rs []libdns.Record) ([]Record, error) {
	var out []Record
	for _, r := range rs {
		rr := r.RR()
		// A CNAME is reported by name only: its target is not ours to read, but a
		// CNAME at a name Core wants makes an A there invalid, so the caller must
		// see it to refuse the name.
		if strings.EqualFold(rr.Type, "CNAME") {
			out = append(out, Record{Name: rr.Name, Type: "CNAME", ProviderID: providerID(r)})
			continue
		}
		parsed, err := rr.Parse()
		if err != nil {
			if rr.Type == "A" || (rr.Type == "SRV" && strings.HasPrefix(rr.Name, "_minecraft._tcp")) {
				return nil, fmt.Errorf("unparseable %s record %q: %w", rr.Type, rr.Name, err)
			}
			continue
		}
		switch v := parsed.(type) {
		case libdns.Address:
			if v.IP.Is4() {
				out = append(out, Record{Name: v.Name, Type: "A", Value: v.IP.String(), ProviderID: providerID(r)})
			}
		case libdns.SRV:
			if v.Service == "minecraft" && v.Transport == "tcp" {
				out = append(out, Record{Name: v.Name, Type: "SRV", Value: strings.TrimSuffix(v.Target, "."), Port: v.Port, ProviderID: providerID(r)})
			}
		}
	}
	return out, nil
}

// providerID renders the opaque ProviderData of the record as the provider
// returned it (RR.Parse drops it, so it is read from the original). None of the
// supported packages attach any today; the field stays empty then.
func providerID(r libdns.Record) string {
	var d any
	switch v := r.(type) {
	case libdns.Address:
		d = v.ProviderData
	case libdns.SRV:
		d = v.ProviderData
	}
	switch v := d.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}
