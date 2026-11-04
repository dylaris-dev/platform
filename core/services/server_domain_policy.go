package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

// Settings of direct-mode server domains. Both limits use the
// ParseLimitSetting format; "" (never saved) is no limit.
const (
	DNSMaxPerUserSettingKey   = "dns.max_subdomains_per_user"
	DNSMaxPerServerSettingKey = "dns.max_subdomains_per_server"
	DNSIntervalSettingKey     = "dns.reconcile_interval_seconds"
	// DNSUserDefaultScope is the dns_subdomain_limits row every user without
	// a row of their own falls back to.
	DNSUserDefaultScope = "user_default"
)

// ValidateSubdomainLabel accepts one lower-case DNS label: 1-63 letters,
// digits and hyphens, not starting or ending with a hyphen. The caller
// lower-cases first. Wildcards and underscores are refused: the first would
// claim every name in the zone, the second is how SRV names start.
func ValidateSubdomainLabel(s string) error {
	switch {
	case s == "":
		return errors.New("enter a subdomain")
	case len(s) > 63:
		return errors.New("a subdomain may have at most 63 characters")
	case s[0] == '-' || s[len(s)-1] == '-':
		return errors.New("a subdomain may not start or end with a hyphen")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return errors.New("a subdomain may only contain letters, digits and hyphens")
		}
	}
	return nil
}

// ValidateFreeFQDN normalises a typed host name (lower case, no trailing dot)
// and checks it: at least two labels, each a valid label, 253 characters at
// most, no scheme, port or IP address.
func ValidateFreeFQDN(s string) (string, error) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	switch {
	case s == "":
		return "", errors.New("enter a domain")
	case strings.Contains(s, "://") || strings.ContainsAny(s, "/:@"):
		return "", errors.New("enter only the host name, for example play.example.com, without http:// or a port")
	case len(s) > 253:
		return "", errors.New("a domain may have at most 253 characters")
	case !strings.Contains(s, "."):
		return "", errors.New("enter a full domain with a dot, for example play.example.com")
	case net.ParseIP(s) != nil:
		return "", errors.New("enter a domain, not an IP address")
	}
	labels := strings.Split(s, ".")
	for _, l := range labels {
		if err := ValidateSubdomainLabel(l); err != nil {
			return "", fmt.Errorf("%q is not a valid domain: every part between dots must be 1-63 letters, digits or hyphens", s)
		}
	}
	if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil {
		return "", errors.New("enter a domain, not an IP address")
	}
	return s, nil
}

// ZonePrincipals is who a user is for zone access: "user:<id>", plus
// "role:<id>" for their panel role.
func ZonePrincipals(st store.Store, userID string) ([]string, error) {
	out := []string{"user:" + userID}
	roleID, _, err := st.GetUserPanelAuthz(userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("read panel role of %s: %w", userID, err)
	}
	if roleID != nil {
		out = append(out, "role:"+strconv.Itoa(*roleID))
	}
	return out, nil
}

// CanUseZone reports whether a holder of principals may create subdomains in
// the zone. Admins always may; a zone without access rows is admins only.
func CanUseZone(principals []string, access []store.DNSZoneAccess, zoneID int, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	for _, a := range access {
		if a.ZoneID != zoneID {
			continue
		}
		for _, p := range principals {
			if a.Principal == p {
				return true
			}
		}
	}
	return false
}

// ValidZonePrincipal accepts "user:<id>" and "role:<n>".
func ValidZonePrincipal(p string) bool {
	kind, id, ok := strings.Cut(p, ":")
	if !ok || id == "" || strings.TrimSpace(id) != id {
		return false
	}
	switch kind {
	case "role":
		n, err := strconv.Atoi(id)
		return err == nil && n > 0
	case "user":
		return len(id) <= 64
	}
	return false
}

// SubdomainLimitFor is how many domains servers owned by ownerID may hold in
// total (nil = no limit): the user:<id> row, else the user_default row, else
// the setting. A row that exists answers, including with NULL. A read error
// is logged and falls through; the setting is always there to answer.
func SubdomainLimitFor(st store.Store, ownerID string) *int64 {
	for _, scope := range []string{"user:" + ownerID, DNSUserDefaultScope} {
		l, err := st.GetDNSSubdomainLimit(scope)
		if err != nil {
			log.Printf("dns limits: read %s: %v", scope, err)
			continue
		}
		if l != nil {
			return l.Max
		}
	}
	return limitSetting(st, DNSMaxPerUserSettingKey)
}

// ServerSubdomainLimit is how many domains one server may hold (nil = no limit).
func ServerSubdomainLimit(st store.Store) *int64 {
	return limitSetting(st, DNSMaxPerServerSettingKey)
}

func limitSetting(st store.Store, key string) *int64 {
	raw, err := st.GetSetting(key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("dns limits: read %s: %v", key, err)
	}
	return ParseLimitSetting(strings.TrimSpace(raw), nil)
}

// DNSReconcileInterval is dns.reconcile_interval_seconds, clamped to [30s, 24h].
func DNSReconcileInterval(st store.Store) time.Duration {
	raw, _ := st.GetSetting(DNSIntervalSettingKey)
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < int(dnsReconcileMinimum/time.Second) {
		return dnsReconcileMinimum
	}
	if n > int(dnsReconcileMaximum/time.Second) {
		return dnsReconcileMaximum
	}
	return time.Duration(n) * time.Second
}

// DirectAddress is what a player types for a server in direct mode,
// "<ip>:<port>", from the node's first public IPv4 (the reconciler's rule).
// "" when the node has none or the server has no port.
func DirectAddress(node *models.Node, hostPort int) string {
	if node == nil || hostPort <= 0 || hostPort > 65535 {
		return ""
	}
	ip := publicIPv4(node.PublicIP, node.Address)
	if !ip.IsValid() {
		return ""
	}
	return ip.String() + ":" + strconv.Itoa(hostPort)
}

// OwnedRecordName is a ledger row's full name, SRV with its _minecraft._tcp.
// prefix, the way an admin finds it at the provider.
func OwnedRecordName(o store.DNSOwnedRecord) string {
	if o.Name == "" {
		return o.Zone
	}
	return o.Name + "." + o.Zone
}

// DeleteDNSAccountRecords removes, through the provider, every record the
// account's ledger owns, so the account can be deleted. It never touches a
// record outside the ledger, nor one whose value is no longer what Core wrote. It returns the names it could not remove (zone
// unreachable, credentials gone): those stay at the provider for an admin to
// delete by hand. The ledger rows themselves go with the account.
func DeleteDNSAccountRecords(ctx context.Context, st store.Store, clusterSecret string, acc store.DNSAccount,
	newProvider func(kind string, creds map[string]string) (dnsprov.Provider, error)) ([]string, error) {
	owned, err := st.ListDNSOwnedRecords()
	if err != nil {
		return nil, fmt.Errorf("list owned records: %w", err)
	}
	byZone := map[string][]store.DNSOwnedRecord{}
	for _, o := range owned {
		if o.AccountID == acc.ID {
			z := strings.ToLower(o.Zone)
			byZone[z] = append(byZone[z], o)
		}
	}
	if len(byZone) == 0 {
		return []string{}, nil
	}
	orphaned := []string{}
	keep := func(rows []store.DNSOwnedRecord) {
		for _, o := range rows {
			orphaned = append(orphaned, OwnedRecordName(o))
		}
	}
	var prov dnsprov.Provider
	creds, err := DecryptDNSCredentials(clusterSecret, acc.EncCredentials)
	if err == nil {
		prov, err = newProvider(acc.Provider, creds)
	}
	if err != nil {
		log.Printf("dns account %d: delete records: %s", acc.ID, dnsprov.Scrub(err, creds))
		for _, rows := range byZone {
			keep(rows)
		}
		sort.Strings(orphaned)
		return orphaned, nil
	}
	for zone, rows := range byZone {
		existing, err := prov.GetRecords(ctx, zone)
		if err != nil {
			log.Printf("dns account %d: read zone %s: %s", acc.ID, zone, dnsprov.Scrub(err, creds))
			keep(rows)
			continue
		}
		// Only a record whose value is still the one Core wrote: a name the
		// owner repointed by hand keeps its record.
		var del []dnsprov.Record
		for _, rec := range existing {
			if ownedByAny(rows, rec) {
				del = append(del, rec)
			}
		}
		if len(del) == 0 {
			continue // already gone at the provider
		}
		if err := prov.DeleteRecords(ctx, zone, del); err != nil {
			log.Printf("dns account %d: delete in zone %s: %s", acc.ID, zone, dnsprov.Scrub(err, creds))
			keep(rows)
		}
	}
	sort.Strings(orphaned)
	return orphaned, nil
}
