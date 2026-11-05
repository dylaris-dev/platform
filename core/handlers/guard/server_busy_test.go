package guard_test

import (
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
)

// Every handler that rewrites or removes a server's directory takes the guard,
// before it sends the node anything. Named per function, so moving the check
// out of one of them fails here by name.
func TestEveryRewritingHandlerTakesTheBusyGuard(t *testing.T) {
	// The guard is matched by name with any package qualifier, so it may
	// move to another package without this list changing.
	for _, c := range []struct{ file, header, guard string }{
		{"../servers/servers_lifecycle.go", "SetupServer", "RefuseIfServerBusy("},
		{"../servers/servers_lifecycle.go", "ReinstallServer", "RefuseIfServerBusy("},
		{"../servers/servers_lifecycle.go", "SwitchSubServer", "RefuseIfServerBusy("},
		{"../servers/servers_lifecycle.go", "DeleteSubServer", "RefuseIfServerBusy("},
		{"../servers/servers_lifecycle.go", "queueMigration", "RefuseIfServerBusy("},
		{"../servers/servers_lifecycle.go", "DeleteServer", "RefuseIfMoving("},
		{"../servers/server_version_update.go", "CopySubServer", "RefuseIfServerBusy("},
		{"../servers/server_version_update.go", "VersionUpdate", "RefuseIfServerBusy("},
		{"../backup/backup.go", "RestoreRun", "RefuseIfServerBusy("},
	} {
		body := handlertest.FuncBody(t, c.file, c.header)
		guard := strings.Index(body, c.guard)
		send := strings.Index(body, "SendCommand(")
		if send < 0 {
			send = strings.Index(body, "EnqueueMigration(")
		}
		if guard < 0 || (send >= 0 && send < guard) {
			t.Errorf("%s: %s missing or after the node is told", c.header, c.guard)
		}
	}
}

// Where a handler takes both guards, the suspended tenant hears 403 and its
// way out, not 409 "the node is busy": the cutoff is the answer that holds
// after the node finishes. Pinned when the guards moved to handlers/guard.
func TestSuspensionIsAnsweredBeforeTheBusyGuard(t *testing.T) {
	for _, c := range []struct{ file, fn string }{
		{"../servers/servers_lifecycle.go", "SetupServer"},
		{"../servers/servers_lifecycle.go", "ReinstallServer"},
		{"../servers/servers_lifecycle.go", "SwitchSubServer"},
		{"../servers/server_version_update.go", "CopySubServer"},
		{"../servers/server_version_update.go", "VersionUpdate"},
		{"../backup/backup.go", "RestoreRun"},
	} {
		body := handlertest.FuncBody(t, c.file, c.fn)
		sus, busy := strings.Index(body, "RefuseIfSuspended("), strings.Index(body, "RefuseIfServerBusy(")
		if sus < 0 || busy < 0 || busy < sus {
			t.Errorf("%s: the busy guard answers before the suspension guard (or one is missing)", c.fn)
		}
	}
}

// Failure paths that used to leave state behind, named per site.
func TestFailedDispatchesLeaveNothingBehind(t *testing.T) {
	for _, c := range []struct{ file, fn, want, why string }{
		{"../servers/servers_lifecycle.go", "CreateServer", "h.d.Store.DeleteServer(int(serverID))", "a create the node never got left a row with no disk limit"},
		{"../servers/servers_lifecycle.go", "SetupServer", "services.RestoreSetupBefore(r.Context(), h.d.Redis, h.d.Store, srv, subName)", "a setup that was never sent stayed 'installing'"},
		{"../servers/servers_lifecycle.go", "SetupServer", "services.RememberSetupBefore(r.Context(), h.d.Redis, h.d.Store, srv, subName)", "a failed install could not be put back"},
		{"../servers/servers_lifecycle.go", "ReinstallServer", "services.RememberSetupBefore(r.Context(), h.d.Redis, h.d.Store, srv, subName)", "a rolled-back reinstall left the DB on the new version"},
		{"../servers/servers_lifecycle.go", "ReinstallServer", "undo()", "a reinstall that was never sent stayed 'installing'"},
		{"../servers/servers_lifecycle.go", "CreateServer", `configPayload["storagePath"] = sp`, "the wizard's storage path was dropped"},
		{"../guard/sub_server_limit.go", "RefuseIfSubServerLimitReached", "if subServerDirExists(entries, target) {", "the cap counted the sub-server being re-installed"},
		{"../servers/servers_lifecycle.go", "ReinstallServer", `h.d.Store.UpdateServerDesiredState(srv.ID, "online")`, "a reinstall of a stopped server would stay down now that the node asks the intent"},
	} {
		if !strings.Contains(handlertest.FuncBody(t, c.file, c.fn), c.want) {
			t.Errorf("%s: %s", c.fn, c.why)
		}
	}
}

// A Beam ticket minted while the server moves carries no write: the source copy
// is deleted after cutover, so a write landing meanwhile was lost.
func TestABeamTicketIsReadOnlyWhileTheServerMoves(t *testing.T) {
	body := handlertest.FuncBody(t, "../beam/beam.go", "GetBeamTicket")
	if !strings.Contains(body, `if server.Status == "migrating" {`) || !strings.Contains(body, "filePerms.Write, filePerms.Delete = false, false") {
		t.Error("a ticket for a moving server still writes")
	}
}

// The node asks the intent when an install finishes; a short install used to
// finish ahead of the scan that published "online" and left the server down.
// Setup and reinstall publish it before they send the command.
func TestInstallsPublishTheIntentBeforeTheCommand(t *testing.T) {
	for fn, action := range map[string]string{"SetupServer": `"setup"`, "ReinstallServer": `"reinstall"`} {
		body := handlertest.FuncBody(t, "../servers/servers_lifecycle.go", fn)
		pub := strings.Index(body, `services.PublishDesiredState(r.Context(), h.d.Redis, srv.UUID, "online")`)
		send := strings.Index(body, "SendCommand(context.Background(), node.Token, "+action)
		if pub < 0 || send < 0 || pub > send {
			t.Errorf("%s publishes the intent after the command, or not at all", fn)
		}
	}
}
