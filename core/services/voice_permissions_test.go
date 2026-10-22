package services

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type voicePermFakeStore struct {
	voiceFakeStore
	perms      map[string]bool
	permErr    map[string]error
	roles      map[string]int // owner -> panel role id
	roleErr    error
	global     string
	globalErr  error
	servers    map[int]*models.Server
	serverErrs map[int]error
}

func (f *voicePermFakeStore) GetVoicePermission(scope string) (*bool, error) {
	if err := f.permErr[scope]; err != nil {
		return nil, err
	}
	if v, ok := f.perms[scope]; ok {
		return &v, nil
	}
	return nil, nil
}

func (f *voicePermFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	if f.roleErr != nil {
		return nil, store.CapOverrides{}, f.roleErr
	}
	if r, ok := f.roles[userID]; ok {
		return &r, store.CapOverrides{}, nil
	}
	return nil, store.CapOverrides{}, nil
}

func (f *voicePermFakeStore) GetSetting(key string) (string, error) {
	if key != VoiceChatSettingKey {
		return "", sql.ErrNoRows
	}
	if f.globalErr != nil {
		return "", f.globalErr
	}
	if f.global == "" {
		return "", sql.ErrNoRows
	}
	return f.global, nil
}

func (f *voicePermFakeStore) GetServerByID(id int) (*models.Server, error) {
	if err := f.serverErrs[id]; err != nil {
		return nil, err
	}
	return f.servers[id], nil
}

func TestResolveVoiceAllowedPrecedence(t *testing.T) {
	srv := &models.Server{ID: 7, OwnerID: "u1"}
	boom := errors.New("db down")
	cases := []struct {
		name        string
		f           voicePermFakeStore
		wantAllowed bool
		wantBy      string
		wantErr     bool
	}{
		{"nothing set is the global default, off", voicePermFakeStore{}, false, "global", false},
		{"the global setting answers when no row does", voicePermFakeStore{global: "true"}, true, "global", false},
		{"a role allow beats a global deny", voicePermFakeStore{roles: map[string]int{"u1": 3}, perms: map[string]bool{"role:3": true}}, true, "role:3", false},
		{"a role deny beats a global allow", voicePermFakeStore{global: "true", roles: map[string]int{"u1": 3}, perms: map[string]bool{"role:3": false}}, false, "role:3", false},
		{"a row for another role does not apply", voicePermFakeStore{global: "true", roles: map[string]int{"u1": 3}, perms: map[string]bool{"role:4": false}}, true, "global", false},
		{"a user deny beats a role allow", voicePermFakeStore{roles: map[string]int{"u1": 3}, perms: map[string]bool{"role:3": true, "user:u1": false}}, false, "user:u1", false},
		{"a user allow beats a role deny", voicePermFakeStore{roles: map[string]int{"u1": 3}, perms: map[string]bool{"role:3": false, "user:u1": true}}, true, "user:u1", false},
		{"a server deny beats a user allow", voicePermFakeStore{global: "true", perms: map[string]bool{"user:u1": true, "server:7": false}}, false, "server:7", false},
		{"a server allow beats a user deny", voicePermFakeStore{perms: map[string]bool{"user:u1": false, "server:7": true}}, true, "server:7", false},
		{"a missing server row defers to the user", voicePermFakeStore{perms: map[string]bool{"server:8": false, "user:u1": true}}, true, "user:u1", false},
		{"an unparseable global reads as off", voicePermFakeStore{global: "yes please"}, false, "global", false},
		{"a scope read error is no", voicePermFakeStore{global: "true", permErr: map[string]error{"user:u1": boom}}, false, "user:u1", true},
		{"a role read error is no", voicePermFakeStore{global: "true", roleErr: boom}, false, "role", true},
		{"a global read error is no", voicePermFakeStore{globalErr: boom}, false, "global", true},
		{"a deleted owner defers past user and role", voicePermFakeStore{global: "true", roleErr: sql.ErrNoRows}, true, "global", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, by, err := ResolveVoiceAllowed(&c.f, srv)
			if allowed != c.wantAllowed || by != c.wantBy || (err != nil) != c.wantErr {
				t.Fatalf("got (%v, %q, %v), want (%v, %q, err=%v)", allowed, by, err, c.wantAllowed, c.wantBy, c.wantErr)
			}
		})
	}
}

func TestResolveVoiceAllowedOwnerlessServerSkipsUserAndRole(t *testing.T) {
	f := &voicePermFakeStore{global: "true", perms: map[string]bool{"user:": false}, roleErr: errors.New("must not be asked")}
	allowed, by, err := ResolveVoiceAllowed(f, &models.Server{ID: 7})
	if !allowed || by != "global" || err != nil {
		t.Fatalf("got (%v, %q, %v), want (true, global, nil)", allowed, by, err)
	}
}

func TestValidVoicePermissionScope(t *testing.T) {
	for scope, want := range map[string]bool{
		"server:7": true, "role:3": true, "user:0b0e6c1e-1111-2222-3333-444444444444": true,
		"global": false, "server:": false, "server:x": false, "server:07": false, "server:0": false,
		"role:-1": false, "user:": false, "user:a b": false, "user:a:b": false, "team:1": false, "": false,
	} {
		if got := ValidVoicePermissionScope(scope); got != want {
			t.Errorf("ValidVoicePermissionScope(%q) = %v, want %v", scope, got, want)
		}
	}
}

func TestReconcileRevokesServersNoLongerAllowed(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	ctx := context.Background()
	allowed := testVoicePort()
	denied := testVoicePort()
	denied.ServerID, denied.ServerUUID, denied.Port = 8, "88888888-0000-0000-0000-000000000000", 26001
	unreadable := testVoicePort()
	unreadable.ServerID, unreadable.ServerUUID, unreadable.Port = 9, "99999999-0000-0000-0000-000000000000", 26002
	for _, p := range []store.VoicePort{allowed, denied, unreadable} {
		if err := PublishVoice(ctx, rdb, p); err != nil {
			t.Fatal(err)
		}
	}
	fs := &voicePermFakeStore{
		voiceFakeStore: voiceFakeStore{rows: []store.VoicePort{allowed, denied, unreadable}},
		global:         "true",
		perms:          map[string]bool{"user:bad": false},
		servers: map[int]*models.Server{
			7: {ID: 7, OwnerID: "good"},
			8: {ID: 8, OwnerID: "bad"},
			9: {ID: 9, OwnerID: "bad"},
		},
		serverErrs: map[int]error{9: errors.New("db blip")},
	}
	var revoked []int
	svc := NewVoiceService(fs, rdb)
	svc.SetRevoke(func(_ context.Context, srv *models.Server) error {
		revoked = append(revoked, srv.ID)
		return nil
	})
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != 8 {
		t.Fatalf("revoked %v, want [8]: only the denied server, never one whose read failed", revoked)
	}
	if mr.Exists("voice:port:eu:26001") || mr.Exists("dylaris:server:"+denied.ServerUUID+":voice") {
		t.Fatal("the revoked server's keys were rewritten")
	}
	for _, p := range []store.VoicePort{allowed, unreadable} {
		if !mr.Exists(VoicePortKey(p.Region, p.Port)) {
			t.Fatalf("server %d lost its keys", p.ServerID)
		}
	}
}

func TestReconcileKeepsTheRowWhenTheRevokeFails(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	fs := &voicePermFakeStore{
		voiceFakeStore: voiceFakeStore{rows: []store.VoicePort{p}},
		servers:        map[int]*models.Server{7: {ID: 7}},
	}
	svc := NewVoiceService(fs, rdb)
	svc.SetRevoke(func(context.Context, *models.Server) error { return errors.New("row delete failed") })
	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(VoicePortKey(p.Region, p.Port)) {
		t.Fatal("a failed revoke must leave the row published, so the next tick asks again")
	}
}

// The resolver answers allowed=false together with a read error. The reconcile
// must read that as "unknown", not "deny": a settings or permission read that
// fails once would otherwise switch voice off on every server at once.
func TestReconcileNeverRevokesOnAPermissionReadError(t *testing.T) {
	boom := errors.New("db blip")
	cases := []struct {
		name string
		f    voicePermFakeStore
	}{
		{"a scope row read fails", voicePermFakeStore{global: "true", permErr: map[string]error{"user:u1": boom}}},
		{"the panel role read fails", voicePermFakeStore{global: "true", roleErr: boom}},
		{"the global setting read fails", voicePermFakeStore{globalErr: boom}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr, rdb := newVoiceRedis(t)
			p := testVoicePort()
			fs := c.f
			fs.voiceFakeStore = voiceFakeStore{rows: []store.VoicePort{p}}
			fs.servers = map[int]*models.Server{7: {ID: 7, OwnerID: "u1"}}
			revoked := 0
			svc := NewVoiceService(&fs, rdb)
			svc.SetRevoke(func(context.Context, *models.Server) error { revoked++; return nil })
			if err := svc.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if revoked != 0 {
				t.Fatalf("revoked on a read error")
			}
			if !mr.Exists(VoicePortKey(p.Region, p.Port)) || !mr.Exists(ServerVoiceKey(p.ServerUUID)) {
				t.Fatalf("keys lost on a read error: %v", mr.Keys())
			}
		})
	}
}
