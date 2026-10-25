package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
)

// Settings of the extra ports feature (direct routing only).
const (
	// PortsEnabledSettingKey is the global answer to "may a server have extra
	// ports", below every port_permissions row. Default false.
	PortsEnabledSettingKey = "ports.extra_enabled"
	// PortsRangeSettingKey is the global host port range ("26000-26099") a
	// non-admin's port is taken from; a node may override it. Empty = no range.
	PortsRangeSettingKey = "ports.extra_range"
	// PortsMaxPerUserSettingKey caps extra ports per server owner, below every
	// port_limits row (ParseLimitSetting; "" = DefaultPortsPerUser).
	PortsMaxPerUserSettingKey = "ports.max_per_user"
	// PortsNodeSince is the first node release that publishes extra ports.
	PortsNodeSince = "2026.10.25"
)

// DefaultPortsPerUser is the product default for ports.max_per_user.
var DefaultPortsPerUser int64 = 2

// The node's own ports (gRPC, metrics, ...); no range or port may touch them.
const nodeOwnPortMin, nodeOwnPortMax = 25520, 25523

// ParsePortRange reads "min-max". It refuses a range outside 1024-65535, one
// touching the node's own ports, and one wider than 1000 ports (a typo such as
// 2000-60000 would otherwise hand out ports over everything on the host).
func ParsePortRange(s string) (min, max int, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return 0, 0, errors.New("write the range as start-end, for example 26000-26099")
	}
	min, err1 := strconv.Atoi(strings.TrimSpace(a))
	max, err2 := strconv.Atoi(strings.TrimSpace(b))
	switch {
	case err1 != nil || err2 != nil:
		return 0, 0, errors.New("the range must be two numbers, for example 26000-26099")
	case min < 1024 || max > 65535:
		return 0, 0, errors.New("the range must lie between 1024 and 65535")
	case min > max:
		return 0, 0, errors.New("the range must start below its end")
	case max-min+1 > 1000:
		return 0, 0, errors.New("a range may hold at most 1000 ports")
	case min <= nodeOwnPortMax && max >= nodeOwnPortMin:
		return 0, 0, fmt.Errorf("the range must not include the node's own ports %d-%d", nodeOwnPortMin, nodeOwnPortMax)
	}
	return min, max, nil
}

// PortRangesOverlap reports whether two "min-max" ranges share a port. An
// unparseable one overlaps nothing (the caller validated its own).
func PortRangesOverlap(a, b string) bool {
	amin, amax, err1 := parseLooseRange(a)
	bmin, bmax, err2 := parseLooseRange(b)
	return err1 == nil && err2 == nil && amin <= bmax && bmin <= amax
}

// parseLooseRange reads "min-max" without ParsePortRange's policy, for ranges
// the node reports (its game port range may be anything it was configured to).
func parseLooseRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return 0, 0, errors.New("no range")
	}
	min, err1 := strconv.Atoi(strings.TrimSpace(a))
	max, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || min > max {
		return 0, 0, errors.New("bad range")
	}
	return min, max, nil
}

// InPortRange reports whether port lies in the "min-max" range.
func InPortRange(port int, r string) bool {
	min, max, err := parseLooseRange(r)
	return err == nil && port >= min && port <= max
}

// ResolvePortsAllowed answers whether srv may have extra ports, and which
// scope decided: server:<id> > user:<owner> > role:<owner's role> > global.
// A read error comes back with allowed=false; the caller fails closed.
func ResolvePortsAllowed(st store.Store, srv *models.Server) (allowed bool, decidedBy string, err error) {
	scopes := []string{"server:" + strconv.Itoa(srv.ID)}
	if srv.OwnerID != "" {
		scopes = append(scopes, "user:"+srv.OwnerID)
		roleID, _, err := st.GetUserPanelAuthz(srv.OwnerID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, "role", fmt.Errorf("read panel role of %s: %w", srv.OwnerID, err)
		}
		if roleID != nil {
			scopes = append(scopes, "role:"+strconv.Itoa(*roleID))
		}
	}
	for _, scope := range scopes {
		v, err := st.GetPortPermission(scope)
		if err != nil {
			return false, scope, fmt.Errorf("read port permission %s: %w", scope, err)
		}
		if v != nil {
			return *v, scope, nil
		}
	}
	raw, err := st.GetSetting(PortsEnabledSettingKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "global", fmt.Errorf("read %s: %w", PortsEnabledSettingKey, err)
	}
	on, _ := strconv.ParseBool(strings.TrimSpace(raw))
	return on, "global", nil
}

// ResolvePortLimit returns the cap on extra ports for servers ownerID owns
// (nil = no limit) and the scope that set it: user:<id> > role:<id> > global.
func ResolvePortLimit(st store.Store, ownerID string) (*int64, string, error) {
	scopes := []string{}
	if ownerID != "" {
		scopes = append(scopes, "user:"+ownerID)
		roleID, _, err := st.GetUserPanelAuthz(ownerID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, "role", fmt.Errorf("read panel role of %s: %w", ownerID, err)
		}
		if roleID != nil {
			scopes = append(scopes, "role:"+strconv.Itoa(*roleID))
		}
	}
	for _, scope := range scopes {
		l, err := st.GetPortLimit(scope)
		if err != nil {
			return nil, scope, fmt.Errorf("read port limit %s: %w", scope, err)
		}
		if l != nil {
			if l.MaxPorts == nil {
				return nil, scope, nil
			}
			v := int64(*l.MaxPorts)
			return &v, scope, nil
		}
	}
	raw, err := st.GetSetting(PortsMaxPerUserSettingKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "global", fmt.Errorf("read %s: %w", PortsMaxPerUserSettingKey, err)
	}
	def := DefaultPortsPerUser
	return ParseLimitSetting(strings.TrimSpace(raw), &def), "global", nil
}

// ValidPortScope accepts server:<id>, user:<id> and role:<id> for
// permissions; limits take user: and role: only (they count per owner).
func ValidPortScope(scope string, limit bool) bool {
	kind, id, ok := strings.Cut(scope, ":")
	if !ok || id == "" || strings.TrimSpace(id) != id {
		return false
	}
	switch kind {
	case "server":
		if limit {
			return false
		}
		n, err := strconv.Atoi(id)
		return err == nil && n > 0
	case "role":
		n, err := strconv.Atoi(id)
		return err == nil && n > 0
	case "user":
		return len(id) <= 64
	}
	return false
}

// ServerPortsKey is where the node reads a server's extra ports from.
func ServerPortsKey(uuid string) string { return "dylaris:server:" + uuid + ":ports" }

// NodePortEntry is one binding the node publishes: protocol, host port,
// container port. The wire names are short and FROZEN: the node compiles its
// own copy (node/extra_ports.go).
type NodePortEntry struct {
	Protocol      string `json:"p"`
	HostPort      int    `json:"h"`
	ContainerPort int    `json:"c"`
}

type nodePortsDoc struct {
	V     int             `json:"v"`
	Ports []NodePortEntry `json:"ports"`
}

// PublishServerPorts writes the node's view of uuid's extra ports, or removes
// the key when there are none. No TTL: Core rewrites it on every change, start
// and reconcile, since this Redis keeps nothing across a restart.
func PublishServerPorts(ctx context.Context, rdb *redis.Client, uuid string, ports []store.ServerPort) error {
	if rdb == nil {
		return nil
	}
	if len(ports) == 0 {
		return rdb.Del(ctx, ServerPortsKey(uuid)).Err()
	}
	doc := nodePortsDoc{V: 1, Ports: make([]NodePortEntry, 0, len(ports))}
	for _, p := range ports {
		doc.Ports = append(doc.Ports, NodePortEntry{Protocol: p.Protocol, HostPort: p.HostPort, ContainerPort: p.ContainerPort})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := rdb.Set(ctx, ServerPortsKey(uuid), b, 0).Err(); err != nil {
		return fmt.Errorf("publish ports of %s: %w", uuid, err)
	}
	return nil
}

// RepublishServerPorts rewrites the node key of every server with extra
// ports. Redis here keeps nothing across a restart, and a node recreates a
// container whenever it is told to, so the key must not depend on the last
// change having been recent.
func RepublishServerPorts(ctx context.Context, st store.Store, rdb *redis.Client) {
	ids, err := st.ListServerIDsWithPorts()
	if err != nil {
		log.Printf("ports republish: %v", err)
		return
	}
	for _, id := range ids {
		srv, err := st.GetServerByID(id)
		if err != nil || srv == nil {
			continue
		}
		ports, err := st.ListServerPorts(id)
		if err != nil {
			log.Printf("ports republish: server %d: %v", id, err)
			continue
		}
		if err := PublishServerPorts(ctx, rdb, srv.UUID, ports); err != nil {
			log.Printf("ports republish: %v", err)
		}
	}
}

// StartPortsRepublisher runs RepublishServerPorts every 60 s on the leader.
func StartPortsRepublisher(ctx context.Context, st store.Store, rdb *redis.Client, l leader.Election) {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			if l == nil || l.IsLeader() {
				RepublishServerPorts(ctx, st, rdb)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
