package guard

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/store"
	pb "dylaris-proto/node"

	"github.com/google/uuid"
)

// ListServerDirFor lists one directory of a server on its node.
//
// It lives here rather than on a handler because the sub-server COUNT is the
// one thing two different handlers have to agree on, and they belong to
// different types. ServerModsHandler.listServerDir delegates to it.
func ListServerDirFor(reg *nodegrpc.Registry, nodeID int, serverUUID, dir string) ([]*pb.FileInfo, error) {
	return listServerDir(reg, nodeID, serverUUID, &pb.ListFilesReq{Path: dir})
}

// ListServerRootNames lists a server's root without folder sizes: what counting
// sub-servers needs, and computing the sizes walks every world on the server.
func ListServerRootNames(reg *nodegrpc.Registry, nodeID int, serverUUID string) ([]*pb.FileInfo, error) {
	return listServerDir(reg, nodeID, serverUUID, &pb.ListFilesReq{NamesOnly: true})
}

func listServerDir(reg *nodegrpc.Registry, nodeID int, serverUUID string, req *pb.ListFilesReq) ([]*pb.FileInfo, error) {
	if reg == nil {
		return nil, fmt.Errorf("node connection not available")
	}
	resp, err := reg.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_ListReq{ListReq: req},
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

// CountSubServerDirs counts the sub-servers in a listing of a server's root.
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

func CountSubServerDirs(entries []*pb.FileInfo) int {
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

// SettingMaxSubServers is the settings key. Named so the setter, the loader and
// the enforcement site in servers_lifecycle.go cannot drift apart by a typo -
// they used to spell it out three times.
const SettingMaxSubServers = "srv.max_sub_servers"

// DefaultMaxSubServers is the product default, used when the setting has never
// been saved. Deliberately a cap and not "unlimited": a fresh install should not
// let one server spawn sub-servers without bound.
var DefaultMaxSubServers = services.LimitPtr(3)

// SubServerLimit is the operator's cap, read through the shared parser so the
// platform limit convention holds: nil means no cap at all, 0 means none.
func SubServerLimit(st store.Store) *int64 {
	if st == nil {
		return nil
	}
	val, err := st.GetSetting(SettingMaxSubServers)
	if err != nil {
		return DefaultMaxSubServers
	}
	return services.ParseLimitSetting(val, DefaultMaxSubServers)
}

// RefuseIfSubServerLimitReached answers the request when the server already
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
func RefuseIfSubServerLimitReached(w http.ResponseWriter, st store.Store, reg *nodegrpc.Registry, nodeID int, serverUUID, target string) bool {
	limit := SubServerLimit(st)
	if limit == nil {
		return false
	}
	entries, err := ListServerRootNames(reg, nodeID, serverUUID)
	if err != nil {
		httpx.SendJSONError(w, "The node did not answer, so the number of sub-servers is not known: "+err.Error(),
			http.StatusBadGateway)
		return true
	}
	if subServerDirExists(entries, target) {
		return false
	}
	return RefuseIfOverSubServerLimit(w, limit, CountSubServerDirs(entries))
}

// RefuseIfOverSubServerLimit is the decision on counts alone, split out so both
// call sites share one message and the rule can be tested without a node.
func RefuseIfOverSubServerLimit(w http.ResponseWriter, limit *int64, have int) bool {
	if !services.AtOrOver(limit, int64(have)) {
		return false
	}
	httpx.SendJSONError(w, fmt.Sprintf("Sub-server limit reached (%d). Change the limit in Settings → Servers.", *limit),
		http.StatusBadRequest)
	return true
}
