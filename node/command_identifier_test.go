package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Both identifiers in a queued command become directory names on this node, so
// a ".." in either escapes the server root. The dangerous branch is
// delete_sub_server, which ends in os.RemoveAll of the joined path.
//
// The guard sits at the dispatcher, so this asserts the property that makes
// that placement correct: the check happens BEFORE the action switch and
// therefore covers every action, including ones added later.
func TestQueuedIdentifiersAreRejectedBeforeAnyActionRuns(t *testing.T) {
	bad := []struct {
		name       string
		uuid       string
		subServer  string
		wantReason string
	}{
		{"traversing uuid", "../neighbour", "survival", "uuid"},
		{"absolute uuid", "/etc", "survival", "uuid"},
		{"uuid with a redis separator", "aaaa:bbbb:cccc", "survival", "uuid"},
		{"empty uuid", "", "survival", "uuid"},
		{"traversing sub-server", "server-1234abcd", "../..", "sub-server"},
		{"sub-server with a slash", "server-1234abcd", "worlds/../../etc", "sub-server"},
		{"sub-server dotfile", "server-1234abcd", ".dylaris-backups", "sub-server"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if commandIdentifierProblem(c.uuid, c.subServer) == "" {
				t.Errorf("uuid=%q sub=%q was accepted; it names a %s directory that escapes the server root",
					c.uuid, c.subServer, c.wantReason)
			}
		})
	}

	good := []struct{ uuid, subServer string }{
		// The panel mints "<ownerUUID>_<random>", not a canonical UUID.
		{"3f8a2b1c-9d4e-4a7b-8c1d-2e5f6a7b8c9d_x9f2", "survival"},
		{"3f8a2b1c-9d4e-4a7b-8c1d-2e5f6a7b8c9d_x9f2", "creative_2"},
		{"3f8a2b1c-9d4e-4a7b-8c1d-2e5f6a7b8c9d_x9f2", "mod-pack+1"},
		// setup and reinstall default an empty name to "server" themselves.
		{"3f8a2b1c-9d4e-4a7b-8c1d-2e5f6a7b8c9d_x9f2", ""},
	}
	for _, c := range good {
		if problem := commandIdentifierProblem(c.uuid, c.subServer); problem != "" {
			t.Errorf("uuid=%q sub=%q was refused (%s), but it is an ordinary command", c.uuid, c.subServer, problem)
		}
	}
}

const testUUID = "3f8a2b1c-9d4e-4a7b-8c1d-2e5f6a7b8c9d_x9f2"

// The guard reads identifiers off a DECODED payload, and Core dispatches in two
// shapes: SendCommand wraps them in "config", SendRawCommand puts them at the
// top level. Testing commandIdentifierProblem on hand-built strings cannot see
// the difference - it is the decode that loses them.
//
// So this drives the real wire payloads. Each literal below is the map Core
// marshals, copied from the call site named above it. When the guard read
// Config alone, every backup and every restore was refused here with `server id
// "" is not a valid identifier` and then ACKed, so Core's run sat at "queued"
// forever with nothing in the node log but one line.
func TestEveryCoreDispatchShapePassesTheIdentifierGuard(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			// core/handlers/backup.go, startBackupRun; identical in
			// core/services/backup_scheduler.go, dispatch. Object storage since
			// document F: a stripped row and upload=multipart, no URL.
			name: "backup_run (flat, SendRawCommand)",
			payload: `{"action":"backup_run","runId":7,"jobId":3,"serverUuid":"` + testUUID + `",
			  "subServer":"survival","includePatterns":["world/**"],"excludePatterns":[],
			  "storageKey":"backups/x.tar.gz","storage":{"id":1,"provider":"s3","config":{}},"upload":"multipart"}`,
		},
		{
			// A whole-server job has sub_server NULL, which Core sends as "".
			name: "backup_run for the whole container",
			payload: `{"action":"backup_run","runId":7,"jobId":3,"serverUuid":"` + testUUID + `",
			  "subServer":"","storageKey":"backups/x.tar.gz","storage":{}}`,
		},
		{
			// core/handlers/backup.go, RestoreBackupRun.
			name: "backup_restore (flat, SendRawCommand)",
			payload: `{"action":"backup_restore","runId":7,"restoreId":2,"jobId":3,
			  "serverUuid":"` + testUUID + `","subServer":"survival",
			  "storageKey":"backups/x.tar.gz","storage":{"id":1,"provider":"connection","config":{}},"download":"presigned"}`,
		},
		{
			// core/handlers/server_mods.go, InstallServerMod (SendCommand).
			name: "install_mod (wrapped in config)",
			payload: `{"action":"install_mod","config":{"uuid":"` + testUUID + `",
			  "activeSubServer":"survival","targetDir":"mods","fileName":"lithium.jar",
			  "downloadUrl":"https://cdn.modrinth.com/x.jar","sha512":""}}`,
		},
		{
			// core/services/queue.go, SendMigrateCommand.
			name: "migrate_storage (config + targetPath)",
			payload: `{"action":"migrate_storage","config":{"uuid":"` + testUUID + `"},
			  "targetPath":"/mnt/disk2/servers"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cmd NodeCommand
			if err := json.Unmarshal([]byte(c.payload), &cmd); err != nil {
				t.Fatalf("Core's payload does not decode into NodeCommand: %v", err)
			}
			uuid, sub := commandIdentifiers(cmd)
			if problem := commandIdentifierProblem(uuid, sub); problem != "" {
				t.Fatalf("the dispatcher refuses a legitimate %s command: %s (uuid=%q sub=%q)",
					cmd.Action, problem, uuid, sub)
			}
			if uuid != testUUID {
				t.Errorf("guard read uuid %q, want %q - it is looking in the wrong field", uuid, testUUID)
			}
		})
	}
}

// The flat shape must be validated, not merely accepted. backup_worker.go and
// backup_restore.go join both identifiers onto a storage path and carry no
// check of their own, so the dispatcher is the only one they get.
func TestFlatPayloadIdentifiersAreValidatedToo(t *testing.T) {
	bad := []struct{ name, payload string }{
		{"traversing serverUuid", `{"action":"backup_restore","serverUuid":"../../etc","subServer":"survival"}`},
		{"traversing subServer", `{"action":"backup_run","serverUuid":"` + testUUID + `","subServer":"../.."}`},
		{"absolute subServer", `{"action":"backup_run","serverUuid":"` + testUUID + `","subServer":"/etc"}`},
		{"no identifiers at all", `{"action":"backup_run","runId":7}`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			var cmd NodeCommand
			if err := json.Unmarshal([]byte(c.payload), &cmd); err != nil {
				t.Fatalf("decode: %v", err)
			}
			uuid, sub := commandIdentifiers(cmd)
			if commandIdentifierProblem(uuid, sub) == "" {
				t.Fatalf("accepted uuid=%q sub=%q; both become directory names under the storage root", uuid, sub)
			}
		})
	}
}

// The flat shape is a cross-module wire contract: Core writes the key names as
// string literals in a map, the node reads them off struct tags, and nothing
// else connects the two. Renaming one side compiles and tests green on that
// side - the symptom is backups going quiet again.
func TestCoreStillNamesTheFlatIdentifiersThisWay(t *testing.T) {
	tagOf := func(field string) string {
		f, ok := reflect.TypeOf(NodeCommand{}).FieldByName(field)
		if !ok {
			t.Fatalf("NodeCommand has no field %s", field)
		}
		return strings.Split(f.Tag.Get("json"), ",")[0]
	}
	uuidKey, subKey := tagOf("ServerUUID"), tagOf("SubServer")

	for _, rel := range []string{
		filepath.Join("..", "core", "handlers", "backup.go"),
		filepath.Join("..", "core", "services", "backup_scheduler.go"),
	} {
		src, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		body := string(src)
		if !strings.Contains(body, "SendRawCommand(") {
			t.Fatalf("%s no longer dispatches a flat payload; this test is watching the wrong file", rel)
		}
		for _, key := range []string{uuidKey, subKey} {
			if !strings.Contains(body, `"`+key+`"`) {
				t.Errorf("%s does not send %q, but NodeCommand reads the identifier from that key", rel, key)
			}
		}
	}
}

// The guard has to be reachable by every action, which means it cannot live in
// a case body. Asserting on the source is the only way to say that: a
// behavioural test can only ever cover the actions that exist today, and the
// failure this prevents is the ELEVENTH action being added without it.
func TestTheIdentifierGuardPrecedesTheActionSwitch(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	guard := strings.Index(body, "commandIdentifierProblem(cmdUUID, cmdSubServer)")
	if guard < 0 {
		t.Fatal("the queued-identifier guard is gone from processCommand; " +
			"both identifiers become directory names and must be validated")
	}
	sw := strings.Index(body, "switch cmd.Action {")
	if sw < 0 {
		t.Fatal("the action switch moved; this test needs updating with it")
	}
	if guard > sw {
		t.Error("the identifier guard now sits inside the action switch, so it only " +
			"protects the branch it landed in; it belongs before the switch")
	}
}
