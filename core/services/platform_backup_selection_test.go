package services

import (
	"errors"
	"testing"

	"dylaris-core/models"
)

// A fleet with every distinction the selection has to make: two owners, and a
// server on a customer's own node.
func fleet() []models.BackupTargetServer {
	return []models.BackupTargetServer{
		{ID: 1, UUID: "u1", Name: "alpha", OwnerID: "alice", NodeID: 10},
		{ID: 2, UUID: "u2", Name: "beta", OwnerID: "bob", NodeID: 10},
		{ID: 3, UUID: "u3", Name: "gamma", OwnerID: "alice", NodeID: 11, BYON: true},
		{ID: 4, UUID: "u4", Name: "delta", OwnerID: "carol", NodeID: 12, BYON: true},
	}
}

func ids(servers []models.BackupTargetServer) []int {
	out := make([]int, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.ID)
	}
	return out
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func str(s string) *string { return &s }

func TestSelectBackupServers(t *testing.T) {
	cases := []struct {
		name        string
		sel         models.PlatformBackupServers
		wantPicked  []int
		wantMissing []int
	}{
		{
			name:       "none takes nothing",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersNone},
			wantPicked: []int{},
		},
		{
			name:       "all takes the fleet",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersAll},
			wantPicked: []int{1, 2, 3, 4},
		},
		{
			// The node's owner decides this, not the server's. Server 3 belongs
			// to alice like server 1 does, and only 3 runs on a customer's box.
			name:       "byon takes only servers on customer hardware",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersBYON},
			wantPicked: []int{3, 4},
		},
		{
			name:       "owner takes every server of one user",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersOwner, OwnerID: str("alice")},
			wantPicked: []int{1, 3},
		},
		{
			// Selecting nobody must not mean selecting everybody. The whole
			// platform archived under a configuration that reads as one user's
			// is the worst possible reading of a missing field.
			name:       "owner with no owner takes nothing",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersOwner},
			wantPicked: []int{},
		},
		{
			name:       "list takes what it names, in the order it names it",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersList, ServerIDs: []int{4, 1}},
			wantPicked: []int{4, 1},
		},
		{
			// The case the whole design turns on: a saved selection outlives
			// what it names. The run must carry on and report the gap.
			name:        "list skips servers that no longer exist",
			sel:         models.PlatformBackupServers{Mode: models.PlatformBackupServersList, ServerIDs: []int{1, 99, 2, 404}},
			wantPicked:  []int{1, 2},
			wantMissing: []int{99, 404},
		},
		{
			name:        "list of nothing but ghosts still picks nothing and fails nothing",
			sel:         models.PlatformBackupServers{Mode: models.PlatformBackupServersList, ServerIDs: []int{99}},
			wantPicked:  []int{},
			wantMissing: []int{99},
		},
		{
			name:       "a repeated id is archived once",
			sel:        models.PlatformBackupServers{Mode: models.PlatformBackupServersList, ServerIDs: []int{2, 2, 2}},
			wantPicked: []int{2},
		},
		{
			// A selection written by a newer version. Archiving on the strength
			// of a word we cannot interpret is the wrong direction to guess in.
			name:       "an unknown mode takes nothing",
			sel:        models.PlatformBackupServers{Mode: "everything-including-the-node"},
			wantPicked: []int{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			picked, missing := SelectBackupServers(fleet(), tc.sel)
			if !equal(ids(picked), tc.wantPicked) {
				t.Errorf("picked = %v, want %v", ids(picked), tc.wantPicked)
			}
			if !equal(missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", missing, tc.wantMissing)
			}
		})
	}
}

// The list mode is the only one that can name something gone, so it is the only
// one that may report anything missing. A query-driven mode reporting a gap
// would mean it invented an id.
func TestSelectBackupServersReportsNothingMissingForQueryModes(t *testing.T) {
	for _, mode := range []models.PlatformBackupServerMode{
		models.PlatformBackupServersNone,
		models.PlatformBackupServersAll,
		models.PlatformBackupServersBYON,
		models.PlatformBackupServersOwner,
	} {
		sel := models.PlatformBackupServers{Mode: mode, OwnerID: str("alice"), ServerIDs: []int{99}}
		if _, missing := SelectBackupServers(fleet(), sel); len(missing) != 0 {
			t.Errorf("mode %q reported %v missing", mode, missing)
		}
	}
}

func TestSelectBackupServersOnAnEmptyFleet(t *testing.T) {
	sel := models.PlatformBackupServers{Mode: models.PlatformBackupServersAll}
	picked, missing := SelectBackupServers(nil, sel)
	if len(picked) != 0 || len(missing) != 0 {
		t.Fatalf("picked=%v missing=%v, want both empty", picked, missing)
	}
}

func TestSkippedServerComponents(t *testing.T) {
	got := SkippedServerComponents([]int{7, 8})
	if len(got) != 2 {
		t.Fatalf("got %d components, want 2", len(got))
	}
	for _, c := range got {
		if c.Kind != "server" {
			t.Errorf("kind = %q, want server", c.Kind)
		}
		// Skipped, not failed: a deleted server is a healthy outcome and must
		// not colour the run red.
		if c.Status != models.PlatformBackupSkipped {
			t.Errorf("status = %q, want skipped", c.Status)
		}
		if c.Ref == "" {
			t.Error("a skipped server with no reference tells the operator nothing")
		}
	}
	if got[0].Ref != "7" || got[1].Ref != "8" {
		t.Errorf("refs = %q, %q; want 7, 8", got[0].Ref, got[1].Ref)
	}
}

func TestPlatformBackupSelectionValidate(t *testing.T) {
	cases := []struct {
		name string
		sel  models.PlatformBackupSelection
		want error
	}{
		{
			name: "database only",
			sel:  models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersNone}},
		},
		{
			name: "servers only",
			sel:  models.PlatformBackupSelection{Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersAll}},
		},
		{
			// A run that archives nothing succeeds and produces a valid, empty
			// bundle, which is indistinguishable from a working backup right up
			// to the moment somebody needs it.
			name: "nothing at all",
			sel:  models.PlatformBackupSelection{Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersNone}},
			want: models.ErrEmptyPlatformBackupSelection,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sel.Validate()
			if tc.want == nil && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// An ABSENT mode and a WRONG one are not the same thing. A payload that omits
// the servers object, or a row written before the field existed, selects no
// servers. A mode that is present but unrecognised is refused - reading it as
// none would silently drop what a newer version meant, and reading it as
// anything else would archive on the strength of a word we cannot interpret.
func TestAnAbsentServerModeIsNoneAndAWrongOneIsRefused(t *testing.T) {
	absent := models.PlatformBackupSelection{Database: true}
	if err := absent.Validate(); err != nil {
		t.Fatalf("a selection with no servers object was refused: %v", err)
	}
	if got := absent.ServerMode(); got != models.PlatformBackupServersNone {
		t.Errorf("ServerMode() = %q, want none", got)
	}
	picked, missing := SelectBackupServers(fleet(), absent.Servers)
	if len(picked) != 0 || len(missing) != 0 {
		t.Errorf("an absent mode selected %v", ids(picked))
	}

	// And absence alone is still an empty run.
	if err := (models.PlatformBackupSelection{}).Validate(); !errors.Is(err, models.ErrEmptyPlatformBackupSelection) {
		t.Errorf("err = %v, want ErrEmptyPlatformBackupSelection", err)
	}

	wrong := models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: "everything"}}
	if err := wrong.Validate(); err == nil {
		t.Error("an unrecognised mode was accepted")
	}
}

func TestPlatformBackupSelectionValidateRejectsIncompleteServerModes(t *testing.T) {
	cases := []struct {
		name string
		sel  models.PlatformBackupSelection
	}{
		{"owner with no owner", models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersOwner}}},
		{"owner with an empty owner", models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersOwner, OwnerID: str("")}}},
		{"list with no servers", models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersList}}},
		{"unknown mode", models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: "byon-but-only-the-good-ones"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.sel.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestPlatformBackupComponentsRoundTrip(t *testing.T) {
	in := []models.PlatformBackupComponent{
		{Kind: "database", Status: models.PlatformBackupIncluded, SizeBytes: 4096},
		{Kind: "server", Ref: "12", Status: models.PlatformBackupSkipped, Message: "gone"},
	}
	got := models.DecodePlatformBackupComponents(models.EncodePlatformBackupComponents(in))
	if len(got) != 2 || got[1].Ref != "12" || got[1].Status != models.PlatformBackupSkipped {
		t.Fatalf("round trip = %+v", got)
	}

	// A nil list must not become the string "null": a reader unmarshalling that
	// gets nil where it expected a list, and every run with no components would
	// read as a run whose record is broken.
	if enc := models.EncodePlatformBackupComponents(nil); enc != "[]" {
		t.Errorf("encoded nil = %q, want []", enc)
	}
	if got := models.DecodePlatformBackupComponents(""); got != nil {
		t.Errorf("decoded empty = %+v, want nil", got)
	}
}
