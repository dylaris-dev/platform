package servers

import (
	"errors"
	"testing"

	"dylaris-core/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of gateway test fixtures that handlers tests still use: the migration
// link-capability test needs the proxy-protocol store without the gateway handler.

// proxyProtoFakeStore backs both PATCH endpoints with a real RedisGateway on
// miniredis, so what is asserted is what the hub, the node and the edge read.
type proxyProtoFakeStore struct {
	store.Store
	settings map[string]string
	servers  map[int]*models.Server
	billing  map[string]*store.UserBilling
	routes   map[string]store.CoreLinkRoute
	setCalls int
}

func (f *proxyProtoFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *proxyProtoFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		c := *s
		return &c, nil
	}
	return nil, errors.New("not found")
}

func (f *proxyProtoFakeStore) SetServerProxyProtocol(id int, enabled bool) error {
	f.setCalls++
	f.servers[id].ProxyProtocol = enabled
	return nil
}

func (f *proxyProtoFakeStore) GetUserBilling(id string) (*store.UserBilling, error) {
	if b, ok := f.billing[id]; ok {
		return b, nil
	}
	return nil, errors.New("no billing")
}

func (f *proxyProtoFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	out := make([]store.CoreLinkRoute, 0, len(f.routes))
	for _, r := range f.routes {
		out = append(out, r)
	}
	return out, nil
}

func (f *proxyProtoFakeStore) UpsertCoreLinkRoute(r store.CoreLinkRoute) error {
	f.routes[r.Domain] = r
	return nil
}

func newProxyProtoHandler(t *testing.T, flagOn bool) (*handlers.AppState, *proxyProtoFakeStore, *redis.Client) {
	t.Helper()
	fs := &proxyProtoFakeStore{
		settings: map[string]string{"routing_mode": "gateway"},
		servers:  map[int]*models.Server{7: {ID: 7, UUID: "srv-7", OwnerID: "owner-1"}},
		billing:  map[string]*store.UserBilling{},
		routes:   map[string]store.CoreLinkRoute{},
	}
	if flagOn {
		fs.settings[services.ProxyProtocolSettingKey] = "true"
	}
	rdb := newLinkRouteRedis(t)
	return &handlers.AppState{
		Store: fs, Redis: rdb, Gateway: services.NewRedisGateway(rdb, fs, "cs"),
		FeatureFlags: services.NewFeatureFlags(fs),
	}, fs, rdb
}

func newLinkRouteRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}
