package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"dylaris-core/pkg/crypto"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

// DomainPaused is the state of every domain while the platform routes through
// the gateway. Its records stay where they are.
const DomainPaused = "paused"

const (
	dnsCredentialPurpose    = "dns-account-credential"
	dnsReconcileMinimum     = 30 * time.Second
	dnsReconcileMaximum     = 24 * time.Hour
	dnsBackoffFirst         = time.Minute
	dnsBackoffMax           = 30 * time.Minute
	srvRecordPrefix         = "_minecraft._tcp."
	domainEventStatusChange = "server_domain.status_changed"
)

// EncryptDNSCredentials seals a provider credential map for dns_accounts.
func EncryptDNSCredentials(clusterSecret string, creds map[string]string) (string, error) {
	if clusterSecret == "" {
		return "", errors.New("no cluster secret to encrypt DNS credentials with")
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("encode dns credentials: %w", err)
	}
	return crypto.Encrypt(crypto.DeriveKey(clusterSecret, dnsCredentialPurpose), raw)
}

// DecryptDNSCredentials reverses EncryptDNSCredentials. Its errors never carry
// any part of the plaintext.
func DecryptDNSCredentials(clusterSecret, enc string) (map[string]string, error) {
	raw, err := crypto.Decrypt(crypto.DeriveKey(clusterSecret, dnsCredentialPurpose), enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt dns credentials: %w", err)
	}
	var creds map[string]string
	if json.Unmarshal(raw, &creds) != nil {
		return nil, errors.New("decode dns credentials: not a credential map")
	}
	return creds, nil
}

type dnsBackoff struct {
	until time.Time
	delay time.Duration
}

// dnsZoneRef is one zone of one account: the unit a pass syncs and backs off.
type dnsZoneRef struct {
	account int
	zone    string
}

// ServerDomainReconciler keeps managed domains' A/SRV records on their node and
// records what DNS actually answers for every domain.
//
// The one rule above all others: it only ever updates or deletes a record that
// is in its own ledger (dns_owned_records). No provider package reports a
// stable record ID, so a record is Core's only when zone, name, type AND value
// (plus port for SRV) match a ledger row: a record someone repointed or added
// by hand at a name Core uses is never touched.
type ServerDomainReconciler struct {
	store         store.Store
	flags         *FeatureFlags
	resolver      ServerDomainResolver
	newProvider   func(kind string, creds map[string]string) (dnsprov.Provider, error)
	clusterSecret string
	leader        leader.Election
	now           func() time.Time
	audit         func(serverID int, event string, details map[string]any)

	// mu serialises passes. backoff is per zone and lives only here, so a
	// leader change costs at most one extra provider call. A zone that errors
	// must not hold back the account's other zones, and their success must not
	// reset its doubling.
	mu        sync.Mutex
	backoff   map[dnsZoneRef]dnsBackoff
	badLabels map[int]bool // domains already logged for an empty label
}

func NewServerDomainReconciler(st store.Store, res ServerDomainResolver, clusterSecret string) *ServerDomainReconciler {
	return &ServerDomainReconciler{
		store: st, flags: NewFeatureFlags(st), resolver: res,
		newProvider: dnsprov.New, clusterSecret: clusterSecret,
		now: time.Now, backoff: map[dnsZoneRef]dnsBackoff{}, badLabels: map[int]bool{},
	}
}

// SetLeader gates each pass; nil means ungated (tests).
func (r *ServerDomainReconciler) SetLeader(l leader.Election) { r.leader = l }

// SetAudit installs the server audit sink. It is a callback because the audit
// package imports services.
func (r *ServerDomainReconciler) SetAudit(fn func(serverID int, event string, details map[string]any)) {
	r.audit = fn
}

func (r *ServerDomainReconciler) shouldRun() bool { return r.leader == nil || r.leader.IsLeader() }

// Interval is dns.reconcile_interval_seconds, clamped to [30s, 24h].
func (r *ServerDomainReconciler) Interval() time.Duration { return DNSReconcileInterval(r.store) }

// Start runs passes until ctx is cancelled, the first one immediately. The
// interval is re-read every pass so a settings change applies without a restart.
func (r *ServerDomainReconciler) Start(ctx context.Context) {
	go func() {
		if r.shouldRun() {
			r.RunOnce(ctx)
		}
		for {
			t := time.NewTimer(r.Interval())
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				if r.shouldRun() {
					r.RunOnce(ctx)
				}
			}
		}
	}()
}

// domainTarget is where a server's players connect in direct mode.
type domainTarget struct {
	ip   netip.Addr // invalid when the node has no public IPv4
	port uint16     // 0 when the server has no host port
}

func (t domainTarget) writable() bool { return t.ip.IsValid() && t.port != 0 }

// recKey names a record the way the ledger does: relative to the zone, SRV with
// its _minecraft._tcp. prefix, lower case.
type recKey struct{ name, typ string }

func keyOf(name, typ string) recKey {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "@" {
		name = ""
	}
	return recKey{name, typ}
}

// providerKey maps a record as dnsprov returns it, where an SRV name is the bare
// label, onto the ledger's name.
func providerKey(rec dnsprov.Record) recKey {
	if rec.Type == "SRV" {
		return keyOf(srvRecordPrefix+rec.Name, "SRV")
	}
	return keyOf(rec.Name, rec.Type)
}

// toProvider turns an ExpectedRecords entry into what dnsprov writes: dnsprov
// adds the _minecraft._tcp. prefix itself, so passing it on would double it.
func toProvider(rec dnsprov.Record) dnsprov.Record {
	if rec.Type == "SRV" {
		rec.Name = strings.TrimPrefix(rec.Name, srvRecordPrefix)
	}
	return rec
}

// normValue is how a record value is stored in the ledger and compared with
// what a provider reports, the same way on both sides: an IP in netip's
// canonical form, anything else lower case without a trailing dot.
func normValue(typ, v string) string {
	v = strings.TrimSpace(v)
	if typ == "A" {
		if a, err := netip.ParseAddr(v); err == nil {
			return a.Unmap().String()
		}
	}
	return strings.ToLower(strings.TrimSuffix(v, "."))
}

// sameValue compares value and, for SRV, port. Names are compared by the caller.
func sameValue(typ, aValue string, aPort uint16, bValue string, bPort uint16) bool {
	return normValue(typ, aValue) == normValue(typ, bValue) && (typ != "SRV" || aPort == bPort)
}

// ownsRecord: the ledger row names exactly this provider record.
func ownsRecord(o store.DNSOwnedRecord, rec dnsprov.Record) bool {
	return keyOf(o.Name, o.Type) == providerKey(rec) && sameValue(rec.Type, o.Value, o.Port, rec.Value, rec.Port)
}

func ownedByAny(rows []store.DNSOwnedRecord, rec dnsprov.Record) bool {
	for _, o := range rows {
		if ownsRecord(o, rec) {
			return true
		}
	}
	return false
}

// ownedAt is the provider record o names, if it is still there.
func ownedAt(have []dnsprov.Record, o store.DNSOwnedRecord) []dnsprov.Record {
	var out []dnsprov.Record
	for _, rec := range have {
		if ownsRecord(o, rec) {
			out = append(out, rec)
		}
	}
	return out
}

func groupByKey(recs []dnsprov.Record) map[recKey][]dnsprov.Record {
	out := map[recKey][]dnsprov.Record{}
	for _, rec := range recs {
		k := providerKey(rec)
		out[k] = append(out[k], rec)
	}
	return out
}

// ledgerRow is the row recording that Core wrote want (an ExpectedRecords entry).
func ledgerRow(accountID int, zone string, want dnsprov.Record, domainID int) store.DNSOwnedRecord {
	k := keyOf(want.Name, want.Type)
	row := store.DNSOwnedRecord{AccountID: accountID, Zone: zone, Name: k.name, Type: k.typ,
		Value: normValue(want.Type, want.Value), ServerDomainID: &domainID}
	if want.Type == "SRV" {
		row.Port = want.Port
	}
	return row
}

func sameRecord(have []dnsprov.Record, want dnsprov.Record) bool {
	return len(have) == 1 && sameValue(want.Type, have[0].Value, have[0].Port, want.Value, want.Port)
}

// recordPlan is one record a domain wants at a name, and the ledger row that
// records it once it is in.
type recordPlan struct {
	k    recKey
	want dnsprov.Record
	row  store.DNSOwnedRecord
}

// publicIPv4 is the operator-set public IP, else the heartbeat address, the
// order the panel uses - but only a public IPv4 counts, since the node's
// self-reported PublicIP can be a NAT-side local address.
func publicIPv4(candidates ...string) netip.Addr {
	for _, c := range candidates {
		a, err := netip.ParseAddr(strings.TrimSpace(c))
		if err != nil {
			continue
		}
		a = a.Unmap()
		if a.Is4() && a.IsGlobalUnicast() && !a.IsPrivate() {
			return a
		}
	}
	return netip.Addr{}
}

func (r *ServerDomainReconciler) targets() (map[int]domainTarget, error) {
	nodes, err := r.store.ListNodes()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	ips := make(map[int]netip.Addr, len(nodes))
	for _, n := range nodes {
		ips[n.ID] = publicIPv4(n.PublicIP, n.Address)
	}
	servers, err := r.store.ListAllServers()
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	out := make(map[int]domainTarget, len(servers))
	for _, s := range servers {
		t := domainTarget{ip: ips[s.NodeID]}
		// HostPort is what the panel shows as the direct join address.
		if s.HostPort > 0 && s.HostPort <= 65535 {
			t.port = uint16(s.HostPort)
		}
		out[s.ID] = t
	}
	return out, nil
}

// RunOnce is one full pass: sync every zone that needs it, then record each
// domain's state from live DNS.
func (r *ServerDomainReconciler) RunOnce(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.flags.IsDNSUpdaterEnabled(ctx) {
		return
	}
	now, interval := r.now(), r.Interval()
	domains, err := r.store.ListServerDomains()
	if err != nil {
		log.Printf("dns reconciler: list domains: %v", err)
		return
	}
	if mode, _ := r.store.GetSetting("routing_mode"); mode == "gateway" {
		for _, d := range domains {
			r.setState(d, DomainStatus{DomainPaused, []string{"paused: gateway routing is on, records are left as they are"}}, now, interval)
		}
		return
	}
	targets, err := r.targets()
	if err != nil {
		log.Printf("dns reconciler: %v", err)
		return
	}
	owned, err := r.store.ListDNSOwnedRecords()
	if err != nil {
		log.Printf("dns reconciler: list owned records: %v", err)
		return
	}
	zones, err := r.store.ListDNSZones()
	if err != nil {
		log.Printf("dns reconciler: list zones: %v", err)
		return
	}
	zoneByID := make(map[int]store.DNSZone, len(zones))
	for _, z := range zones {
		zoneByID[z.ID] = z
	}

	type zoneWork struct {
		domains []store.ServerDomain
		owned   []store.DNSOwnedRecord
		orphans bool
	}
	work := map[dnsZoneRef]*zoneWork{}
	get := func(k dnsZoneRef) *zoneWork {
		if work[k] == nil {
			work[k] = &zoneWork{}
		}
		return work[k]
	}
	blocked := map[int]DomainStatus{}
	for _, d := range domains {
		if d.Mode != store.DomainModeManaged || d.ZoneID == nil {
			continue
		}
		if strings.TrimSpace(d.Label) == "" {
			// The API never stores one; without a label the records would land
			// on the zone apex.
			if !r.badLabels[d.ID] {
				r.badLabels[d.ID] = true
				log.Printf("dns reconciler: managed domain %d (%s) has no label; skipped", d.ID, d.FQDN)
			}
			blocked[d.ID] = DomainStatus{DomainGrey, []string{"invalid domain label"}}
			continue
		}
		// A zone the account no longer exposes is left alone: losing access
		// never deletes records.
		if z, ok := zoneByID[*d.ZoneID]; ok {
			w := get(dnsZoneRef{z.AccountID, strings.ToLower(z.Name)})
			w.domains = append(w.domains, d)
		}
	}
	for _, o := range owned {
		w := get(dnsZoneRef{o.AccountID, strings.ToLower(o.Zone)})
		w.owned = append(w.owned, o)
		w.orphans = w.orphans || o.ServerDomainID == nil
	}
	// A zone with nothing left to sync keeps no backoff: it would hold its
	// account's error up forever.
	for k := range r.backoff {
		if w := work[k]; w == nil || (len(w.domains) == 0 && !w.orphans) {
			delete(r.backoff, k)
		}
	}
	refs := make([]dnsZoneRef, 0, len(work))
	for k, w := range work {
		if len(w.domains) > 0 || w.orphans {
			refs = append(refs, k)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].account != refs[j].account {
			return refs[i].account < refs[j].account
		}
		return refs[i].zone < refs[j].zone
	})

	for _, k := range refs {
		w := work[k]
		for id, st := range r.syncZone(ctx, k, w.domains, w.owned, targets, now) {
			blocked[id] = st
		}
	}

	for _, d := range domains {
		st, ok := blocked[d.ID]
		if !ok {
			st = r.evaluate(ctx, d, targets)
		}
		r.setState(d, st, now, interval)
	}
}

func (r *ServerDomainReconciler) evaluate(ctx context.Context, d store.ServerDomain, targets map[int]domainTarget) DomainStatus {
	t, ok := targets[d.ServerID]
	if !ok {
		return DomainStatus{DomainGrey, []string{"the server no longer exists"}}
	}
	if t.port == 0 {
		return DomainStatus{DomainGrey, []string{"the server has no direct port yet"}}
	}
	return EvaluateDomain(ctx, r.resolver, d.FQDN, t.ip, t.port)
}

// syncZone makes one zone match its managed domains with at most one GetRecords,
// one SetRecords and one DeleteRecords. It returns the domains it refused to
// write for, with the state to show.
func (r *ServerDomainReconciler) syncZone(ctx context.Context, ref dnsZoneRef,
	domains []store.ServerDomain, owned []store.DNSOwnedRecord, targets map[int]domainTarget, now time.Time) map[int]DomainStatus {
	if b, ok := r.backoff[ref]; ok && now.Before(b.until) {
		return nil
	}
	accountID, zone := ref.account, ref.zone
	acc, err := r.store.GetDNSAccount(accountID)
	if err != nil {
		log.Printf("dns reconciler: account %d: %v", accountID, err)
		return nil
	}
	if acc == nil {
		return nil
	}
	creds, err := DecryptDNSCredentials(r.clusterSecret, acc.EncCredentials)
	if err != nil {
		r.fail(acc, ref, "stored credentials cannot be decrypted; enter them again", now)
		return nil
	}
	prov, err := r.newProvider(acc.Provider, creds)
	if err != nil {
		r.fail(acc, ref, dnsprov.Scrub(err, creds), now)
		return nil
	}
	existing, err := prov.GetRecords(ctx, zone)
	if err != nil {
		r.fail(acc, ref, dnsprov.Scrub(err, creds), now)
		return nil
	}
	have := groupByKey(existing)
	rowsAt := map[recKey][]store.DNSOwnedRecord{} // the ledger, by name
	liveOwned := map[recKey]bool{}                // a row that still belongs to a domain
	for _, o := range owned {
		k := keyOf(o.Name, o.Type)
		rowsAt[k] = append(rowsAt[k], o)
		if o.ServerDomainID != nil {
			liveOwned[k] = true
		}
	}
	// foreign: the name holds a record no ledger row names by value.
	foreign := func(k recKey) bool {
		for _, rec := range have[k] {
			if !ownedByAny(rowsAt[k], rec) {
				return true
			}
		}
		return false
	}

	blocked := map[int]DomainStatus{}
	claimed := map[recKey]bool{}
	var toSet, toDelete []dnsprov.Record
	var plans, setPlans []recordPlan
	var dropAfter []int
	for _, d := range domains {
		t := targets[d.ServerID]
		if !t.writable() {
			continue // nothing to point at yet; existing records stay
		}
		want := ExpectedRecords(d.Label, zone, t.ip, t.port)
		conflict := ""
		for _, e := range want {
			name := d.FQDN
			if e.Type == "SRV" {
				name = srvRecordPrefix + d.FQDN
			}
			// Core never writes a CNAME, so one at a name it wants is always
			// foreign - and it makes any other record at that name invalid.
			if len(have[keyOf(e.Name, "CNAME")]) > 0 {
				conflict = foreignRecordProblem(name, "CNAME")
				break
			}
			// Anything at the name that is not exactly what Core wrote - a record
			// made by hand, or Core's own one repointed - blocks the write:
			// SetRecords replaces every record of a name and type.
			if k := keyOf(e.Name, e.Type); foreign(k) {
				if len(rowsAt[k]) > 0 {
					conflict = changedByHandProblem(name, e.Type)
				} else {
					conflict = foreignRecordProblem(name, e.Type)
				}
				break
			}
		}
		if conflict != "" {
			blocked[d.ID] = DomainStatus{DomainYellow, []string{conflict}}
			continue
		}
		wanted := map[recKey]bool{}
		for _, e := range want {
			k := keyOf(e.Name, e.Type)
			wanted[k], claimed[k] = true, true
			p := recordPlan{k: k, want: e, row: ledgerRow(accountID, zone, e, d.ID)}
			plans = append(plans, p)
			if !sameRecord(have[k], e) {
				toSet = append(toSet, toProvider(e))
				setPlans = append(setPlans, p)
			}
		}
		// A record this domain no longer needs (the SRV once the port is 25565):
		// only the value Core wrote goes, a record added there by hand stays.
		for _, o := range owned {
			if k := keyOf(o.Name, o.Type); o.ServerDomainID != nil && *o.ServerDomainID == d.ID && !wanted[k] {
				toDelete = append(toDelete, ownedAt(have[k], o)...)
				dropAfter = append(dropAfter, o.ID)
			}
		}
	}
	for _, o := range owned {
		k := keyOf(o.Name, o.Type)
		// A live domain holds the name: its plan settles this row once its write
		// is in, and until then the row keeps the record Core's.
		if o.ServerDomainID != nil || claimed[k] || liveOwned[k] {
			continue
		}
		// Only the exact value Core wrote is deleted. A name repointed by hand
		// keeps its record and only loses the row.
		toDelete = append(toDelete, ownedAt(have[k], o)...)
		dropAfter = append(dropAfter, o.ID)
	}

	if len(toSet) > 0 {
		if _, err := prov.SetRecords(ctx, zone, toSet); err != nil {
			r.ledgerPartialWrite(ctx, prov, zone, creds, rowsAt, setPlans)
			r.fail(acc, ref, dnsprov.Scrub(err, creds), now)
			return blocked
		}
	}
	// A ledger row only ever follows a successful write. A row for a record that
	// was never written would make Core claim whatever someone creates under that
	// name later. The failure the other way round is the safe one: the record
	// then reads as changed by hand and is never touched again.
	for _, p := range plans {
		r.settleKey(p.row, rowsAt[p.k])
	}
	if len(toDelete) > 0 {
		if err := prov.DeleteRecords(ctx, zone, toDelete); err != nil {
			r.fail(acc, ref, dnsprov.Scrub(err, creds), now)
			return blocked
		}
	}
	r.dropRows(dropAfter)
	delete(r.backoff, ref)
	if acc.LastError != "" && !r.accountBackingOff(accountID) {
		if err := r.store.SetDNSAccountError(accountID, ""); err != nil {
			log.Printf("dns reconciler: clear error of account %d: %v", accountID, err)
		}
	}
	return blocked
}

// settleKey makes the ledger at one name what the provider now holds there:
// exactly row's record. The provider held only Core's values at that name
// (anything else blocks the domain) and SetRecords replaced them all, so every
// other row there names a record that is gone.
func (r *ServerDomainReconciler) settleKey(row store.DNSOwnedRecord, rows []store.DNSOwnedRecord) {
	have := false
	var stale []int
	for _, o := range rows {
		if o.Value == row.Value && o.Port == row.Port {
			have = o.ServerDomainID != nil && *o.ServerDomainID == *row.ServerDomainID
		} else {
			stale = append(stale, o.ID)
		}
	}
	if !have && !r.addOwned(row) {
		return // the old rows stay: they are the only proof left
	}
	r.dropRows(stale)
}

func (r *ServerDomainReconciler) addOwned(row store.DNSOwnedRecord) bool {
	if err := r.store.AddDNSOwnedRecord(row); err != nil {
		log.Printf("dns reconciler: wrote %s record %q in zone %s (account %d) but could not record it as ours: %v; "+
			"it now reads as changed by hand and blocks its domain until an admin deletes it at the provider",
			row.Type, row.Name, row.Zone, row.AccountID, err)
		return false
	}
	return true
}

// ledgerPartialWrite runs after a failed SetRecords. Providers write record by
// record (Cloudflare does), so some may be in, and without a row each would
// read as changed by hand forever. A record is ledgered only when its name now
// holds the exact value sent and nothing beside it but Core's own values. Old
// rows stay; the next pass settles them.
func (r *ServerDomainReconciler) ledgerPartialWrite(ctx context.Context, prov dnsprov.Provider, zone string,
	creds map[string]string, rowsAt map[recKey][]store.DNSOwnedRecord, plans []recordPlan) {
	now, err := prov.GetRecords(ctx, zone)
	if err != nil {
		log.Printf("dns reconciler: re-read zone %s after a failed write: %s", zone, dnsprov.Clip(dnsprov.Scrub(err, creds)))
		return
	}
	have := groupByKey(now)
	for _, p := range plans {
		found, clean := false, true
		for _, rec := range have[p.k] {
			switch {
			case sameValue(p.want.Type, rec.Value, rec.Port, p.want.Value, p.want.Port):
				found = true
			case !ownedByAny(rowsAt[p.k], rec):
				clean = false
			}
		}
		if found && clean {
			r.addOwned(p.row)
		}
	}
}

func (r *ServerDomainReconciler) dropRows(ids []int) {
	for _, id := range ids {
		if err := r.store.DeleteDNSOwnedRecord(id); err != nil {
			log.Printf("dns reconciler: drop owned record %d: %v", id, err)
		}
	}
}

// fail records a scrubbed provider error and backs the account off: 1m,
// doubling, capped at 30m.
func (r *ServerDomainReconciler) fail(acc *store.DNSAccount, ref dnsZoneRef, msg string, now time.Time) {
	b := r.backoff[ref]
	b.delay = min(max(b.delay*2, dnsBackoffFirst), dnsBackoffMax)
	b.until = now.Add(b.delay)
	r.backoff[ref] = b
	msg = dnsprov.Clip(ref.zone + ": " + msg)
	log.Printf("dns reconciler: account %d (%s): %s; retrying the zone in %v", acc.ID, acc.Provider, msg, b.delay)
	if err := r.store.SetDNSAccountError(acc.ID, msg); err != nil {
		log.Printf("dns reconciler: record error of account %d: %v", acc.ID, err)
	}
}

// accountBackingOff reports whether any zone of the account is still failing,
// so one zone's success does not clear the error another zone set.
func (r *ServerDomainReconciler) accountBackingOff(accountID int) bool {
	for k := range r.backoff {
		if k.account == accountID {
			return true
		}
	}
	return false
}

// changedByHandProblem: the name holds a record Core did not write although
// Core's ledger has one there - its own record repointed, or another added
// beside it, at the provider.
func changedByHandProblem(name, typ string) string {
	return fmt.Sprintf("the %s record of %s was changed at the DNS provider by hand - DYLARIS leaves it alone "+
		"until the changed record is removed", typ, name)
}

func foreignRecordProblem(name, typ string) string {
	article := "an"
	if typ == "CNAME" {
		article = "a"
	}
	return fmt.Sprintf("%s already has %s %s record that DYLARIS did not create - remove it or pick another name", name, article, typ)
}

// setState writes only a change, or a refresh once the last observation is an
// interval old.
func (r *ServerDomainReconciler) setState(d store.ServerDomain, st DomainStatus, now time.Time, interval time.Duration) {
	changed := d.State != st.State || !slices.Equal(d.Problems, st.Problems)
	if !changed && d.ObservedAt != nil && now.Sub(*d.ObservedAt) < interval {
		return
	}
	if err := r.store.SetServerDomainState(d.ID, st.State, st.Problems, now); err != nil {
		log.Printf("dns reconciler: state of domain %d: %v", d.ID, err)
		return
	}
	if d.State != st.State && (st.State == DomainYellow || st.State == DomainGreen) && r.audit != nil {
		r.audit(d.ServerID, domainEventStatusChange, map[string]any{"fqdn": d.FQDN, "state": st.State})
	}
}
