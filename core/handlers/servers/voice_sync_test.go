package servers

import (
	"context"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

func TestSyncVoiceWithoutRowTouchesNothing(t *testing.T) {
	fs := &voiceSyncFakeStore{}
	// A nil GRPCRegistry fails any node call, so true means none was made.
	h := NewServerHandler(testDeps(Deps{Store: fs}))
	if !h.syncVoiceToSubServer(&models.Server{ID: 7, UUID: "u"}, "survival") {
		t.Fatal("a server without a voice row reported a failed sync")
	}
	if fs.calls != 1 {
		t.Fatalf("voice row read %d times, want 1", fs.calls)
	}
}

func TestSyncVoiceWithRowReachesForTheNode(t *testing.T) {
	fs := &voiceSyncFakeStore{row: &store.VoicePort{ServerID: 7, Region: "eu", Port: 26000, Host: "a.eu.dylaris.com"}}
	h := NewServerHandler(testDeps(Deps{Store: fs}))
	if h.syncVoiceToSubServer(&models.Server{ID: 7, UUID: "u"}, "survival") {
		t.Fatal("a voice row with no reachable node reported success")
	}
}

func TestSyncVoiceRaceNeverCreatesOrPublishes(t *testing.T) {
	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	moved, movedPort, same := row, row, row
	moved.Region = "us"
	movedPort.Port = 26001
	cases := []struct {
		name        string
		current     *store.VoicePort
		wantPublish bool
	}{
		{"a row deleted by a disable after the read is not resurrected", nil, false},
		{"a row moved to another region after the read is not reverted", &moved, false},
		{"a row moved to another port after the read is not reverted", &movedPort, false},
		{"a row that is still there is refreshed and published", &same, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &voiceRaceStore{row: row, current: c.current}
			st, mr := voiceRaceState(t, fs)
			ok := NewServerHandler(st).syncVoiceToSubServer(&models.Server{ID: 7, UUID: "srv-7", NodeID: 7}, "survival")
			if ok != c.wantPublish {
				t.Fatalf("sync = %v, want %v", ok, c.wantPublish)
			}
			if fs.allocs != 0 {
				t.Fatalf("AllocateVoicePort called %d times; the re-assert must never insert or move a row", fs.allocs)
			}
			if fs.updates != 1 {
				t.Fatalf("UpdateVoicePortMod called %d times, want 1", fs.updates)
			}
			if got := len(mr.Keys()) > 0; got != c.wantPublish {
				t.Fatalf("published = %v (keys %v), want %v", got, mr.Keys(), c.wantPublish)
			}
		})
	}
}

func TestSyncVoiceWritesTheDetectedModsMerge(t *testing.T) {
	cases := []struct {
		name, path, before, after, mod string
	}{
		{"svc gets the properties merge", "plugins/voicechat/voicechat-server.properties", "svc_before.properties", "svc_after.properties", "svc"},
		{"plasmo gets the toml merge", "config/plasmovoice/server/config.toml", "plasmo_default.toml", "plasmo_after.toml", "plasmo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
			fs := &voiceRaceStore{row: row, current: &row}
			st, _, node := voiceNodeState(t, fs, c.path, c.before)
			if !NewServerHandler(st).syncVoiceToSubServer(&models.Server{ID: 7, UUID: "srv-7", NodeID: 7}, "survival") {
				t.Fatal("sync failed")
			}
			got := node.WrittenString()
			if want := handlertest.VoiceFixture(t, c.after); got != want {
				t.Fatalf("wrote:\n%s\nwant:\n%s", got, want)
			}
			if fs.mod != c.mod {
				t.Fatalf("stored mod %q, want %q", fs.mod, c.mod)
			}
		})
	}
}

// voiceReleaseStore records the release a missing voice mod triggers.
type voiceReleaseStore struct {
	voiceRaceStore
	deleted int
}

func (f *voiceReleaseStore) DeleteVoicePort(int) error { f.deleted++; return nil }

// A sub-server that starts without the voice mod (removed, or a switch to one
// that never had it) gives its port back instead of holding it forever.
func TestSyncVoiceReleasesThePortWhenTheModIsGone(t *testing.T) {
	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	fs := &voiceReleaseStore{voiceRaceStore: voiceRaceStore{row: row, current: &row}}
	// The node answers 404 for every voice config path: no mod installed.
	st, mr, _ := voiceNodeState(t, fs, "not/a/voice/config.txt", "svc_before.properties")
	if err := services.PublishVoice(context.Background(), st.Redis, row); err != nil {
		t.Fatal(err)
	}
	if NewServerHandler(st).syncVoiceToSubServer(&models.Server{ID: 7, UUID: "srv-7", NodeID: 7}, "survival") {
		t.Fatal("a sync without the mod reported success")
	}
	if fs.deleted != 1 {
		t.Fatalf("DeleteVoicePort called %d times, want 1", fs.deleted)
	}
	if fs.updates != 0 || fs.allocs != 0 {
		t.Fatalf("updates %d allocs %d: a release must not also refresh the row", fs.updates, fs.allocs)
	}
	if len(mr.Keys()) != 0 {
		t.Fatalf("relay mapping left behind: %v", mr.Keys())
	}
}

// A start writes the mod host: an active white-label name, our name in every
// other state. The relay mapping keeps our name either way.
func TestSyncVoiceWritesTheModHost(t *testing.T) {
	const ours, name = "voice.eu.dylaris.com", "voice.play.example.com"
	for _, c := range []struct {
		state, want string
	}{
		{"", ours},
		{store.VoicePublicPending, ours},
		{store.VoicePublicActive, name},
		{store.VoicePublicFallenBack, ours},
	} {
		t.Run("state "+c.state, func(t *testing.T) {
			row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: ours, UplinkKey: make([]byte, 32)}
			if c.state != "" {
				row.PublicHost, row.PublicState = name, c.state
			}
			fs := &voiceRaceStore{row: row, current: &row}
			st, mr, node := voiceNodeState(t, fs, "plugins/voicechat/voicechat-server.properties", "svc_before.properties")
			if !NewServerHandler(st).syncVoiceToSubServer(&models.Server{ID: 7, UUID: "srv-7", NodeID: 7}, "survival") {
				t.Fatal("sync failed")
			}
			if got := node.WrittenString(); !strings.Contains(got, "voice_host="+c.want+":26000") {
				t.Fatalf("wrote:\n%s\nwant voice_host=%s:26000", got, c.want)
			}
			if key, _ := mr.Get("dylaris:server:srv-7:voice"); !strings.Contains(key, `"host":"`+ours+`"`) {
				t.Fatalf("relay mapping %s", key)
			}
		})
	}
}
