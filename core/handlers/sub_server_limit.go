package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"
	pb "dylaris-proto/node"

	"github.com/google/uuid"
)

// listServerDirFor lists one directory of a server on its node.
//
// It lives here rather than on a handler because the sub-server COUNT is the
// one thing two different handlers have to agree on, and they belong to
// different types. ServerModsHandler.listServerDir delegates to it.
func listServerDirFor(state *AppState, nodeID int, serverUUID, dir string) ([]*pb.FileInfo, error) {
	if state == nil || state.GRPCRegistry == nil {
		return nil, fmt.Errorf("node connection not available")
	}
	resp, err := state.GRPCRegistry.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: dir}},
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if e := resp.GetError(); e != nil {
		// Every caller here asks for a folder that may legitimately not exist
		// (mods/ on a vanilla server). Nodes before 2026.10.11.7 answered that
		// with an empty list and newer ones with a 404; both mean "nothing".
		if e.Code == 404 {
			return []*pb.FileInfo{}, nil
		}
		return nil, fmt.Errorf("%s", e.Message)
	}
	list := resp.GetListResp()
	if list == nil {
		return nil, fmt.Errorf("unexpected response from node")
	}
	return list.Files, nil
}

// countSubServerDirs counts the sub-servers in a listing of a server's root.
// A sub-server IS a directory there; files (.active_server) and anything hidden
// are not sub-servers and must not be counted as one.
//
// Pure, so the counting rule can be exercised without a node on the other end.
// subServerDirExists reports whether target is already one of the sub-server
// directories, exactly: the node's filesystem is case-sensitive, so "SERVER"
// beside "server" is a new directory and must count against the cap.
func subServerDirExists(entries []*pb.FileInfo, target string) bool {
	for _, e := range entries {
		if e != nil && e.IsDir && !strings.HasPrefix(e.Name, ".") && e.Name == target {
			return true
		}
	}
	return false
}

func countSubServerDirs(entries []*pb.FileInfo) int {
	n := 0
	for _, e := range entries {
		if e == nil || !e.IsDir {
			continue
		}
		name := strings.TrimSpace(e.Name)
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		n++
	}
	return n
}

// subServerLimit is the operator's cap, read through the shared parser so the
// platform limit convention holds: nil means no cap at all, 0 means none.
func subServerLimit(state *AppState) *int64 {
	if state == nil || state.Store == nil {
		return nil
	}
	val, err := state.Store.GetSetting(SettingMaxSubServers)
	if err != nil {
		return defaultMaxSubServers
	}
	return services.ParseLimitSetting(val, defaultMaxSubServers)
}

// refuseIfSubServerLimitReached answers the request when the server already
// holds as many sub-servers as it may, and reports whether it did.
//
// The count comes from the NODE, because the node's disk is the only thing that
// knows. It used to come from the disk-stats cache in Redis, and that cache is
// written by a running server - so the cap silently did not apply while a
// server was stopped, which is exactly when sub-servers are added. Measured on
// production: a platform with a limit of three ended up with seven, partly
// through setup while stopped and partly through copy, which never asked at
// all.
//
// A node that does not answer is a refusal, not an assumption. A cap nobody can
// evaluate must not quietly pass - and if the node is unreachable the operation
// behind this check was going to fail anyway.
//
// A setup into a sub-server that already exists adds none, so it passes: the
// cap counted the target itself, which refused re-installing an existing
// sub-server at the limit and gave an upload install - whose file is staged
// into the new folder before setup asks - one fewer than the limit.
func refuseIfSubServerLimitReached(w http.ResponseWriter, state *AppState, nodeID int, serverUUID, target string) bool {
	limit := subServerLimit(state)
	if limit == nil {
		return false
	}
	entries, err := listServerDirFor(state, nodeID, serverUUID, "")
	if err != nil {
		sendJSONError(w, "The node did not answer, so the number of sub-servers is not known: "+err.Error(),
			http.StatusBadGateway)
		return true
	}
	if subServerDirExists(entries, target) {
		return false
	}
	return refuseIfOverSubServerLimit(w, limit, countSubServerDirs(entries))
}

// refuseIfOverSubServerLimit is the decision on counts alone, split out so both
// call sites share one message and the rule can be tested without a node.
func refuseIfOverSubServerLimit(w http.ResponseWriter, limit *int64, have int) bool {
	if !services.AtOrOver(limit, int64(have)) {
		return false
	}
	sendJSONError(w, fmt.Sprintf("Sub-server limit reached (%d). Change the limit in Settings → Servers.", *limit),
		http.StatusBadRequest)
	return true
}
