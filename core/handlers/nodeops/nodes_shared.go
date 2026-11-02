package nodeops

import (
	"context"
	pb "dylaris-proto/node"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
)

// SetNodesLinkOnline fills LinkOnline: is the Link that carries each node's
// players actually connected? The heartbeat's linkCount says how many Link
// containers the node sees on its own host, which stays 1 while that Link
// reaches no edge and nobody can join. This asks the Link itself, through the
// key it refreshes, about the token this node's ROUTES use - the Hub-named one
// where there is one, so a machine served by a self-enrolled Link is not
// reported as dead. Shared by /api/nodes and the infrastructure overview.
//
// Only where the answer can be trusted: a machine whose Link the Hub has
// named, and a customer's machine, whose Link boots with the token Core
// derives (link-boot hands it out). An in-cluster machine served by a
// self-enrolled Link runs under a Hub-GENERATED token, so asking about the
// derived one would report it as not connected for as long as the learner
// has no answer - a red badge on a machine that is serving players.
func SetNodesLinkOnline(ctx context.Context, rdb *redis.Client, clusterSecret string, nodes []models.Node) {
	if len(nodes) == 0 {
		return
	}
	askFor := func(n *models.Node) string {
		if n.LinkToken == "" && !IsBYONNode(*n) {
			return ""
		}
		return services.EffectiveLinkToken(n, clusterSecret)
	}
	tokens := make([]string, 0, len(nodes))
	for i := range nodes {
		if t := askFor(&nodes[i]); t != "" {
			tokens = append(tokens, t)
		}
	}
	online := services.LinkOnline(ctx, rdb, tokens)
	if online == nil {
		return
	}
	for i := range nodes {
		t := askFor(&nodes[i])
		if t == "" {
			continue
		}
		if v, ok := online[t]; ok {
			nodes[i].LinkOnline = &v
		}
	}
}

// InspectOrphanOnNode sends an inspect_orphan gRPC request to nodeID for orphanUUID
// and returns the parsed response. Returns nil, error on gRPC failure or missing orphan.
// Package-level (not a NodeHandler method) so the server-lifecycle switch path can
// reuse it to refresh a server's loader metadata from the node's .dylaris.json.
func InspectOrphanOnNode(reg *nodegrpc.Registry, nodeID int, orphanUUID string) (*pb.InspectOrphanResp, error) {
	resp, err := reg.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: orphanUUID,
		Payload:    &pb.NodeMessage_InspectOrphanReq{InspectOrphanReq: &pb.InspectOrphanReq{}},
	}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("node communication error: %w", err)
	}
	if errResp := resp.GetError(); errResp != nil {
		code := NodeErrorStatus(errResp.Code)
		if code == 0 {
			code = 500
		}
		return nil, fmt.Errorf("node error %d: %s", code, errResp.Message)
	}
	orphanInfo := resp.GetInspectOrphanResp()
	if orphanInfo == nil {
		return nil, fmt.Errorf("orphan folder not found or inspect failed")
	}
	return orphanInfo, nil
}

// SubServerLoaderFromInspect extracts one sub-server's loader/version/build from
// a node's inspect_orphan response. It prefers the richer .dylaris.json metadata
// (which carries minecraft_version and build) and falls back to the directory
// scan, which reports the type only. ok reports whether the sub-server was found
// at all, so callers can leave existing values untouched instead of wiping them
// when the node cannot describe the sub-server.
//
// Shared by orphan adoption (first fill) and the sub-server switch (refresh), so
// both derive installer_type/minecraft_version/build_number the same way and
// cannot drift apart.
func SubServerLoaderFromInspect(resp *pb.InspectOrphanResp, subName string) (instType, mcVer, build string, ok bool) {
	if resp == nil || subName == "" {
		return "", "", "", false
	}
	if resp.HasMetadata && resp.MetadataJson != "" {
		var meta DylarisMetadata
		if err := json.Unmarshal([]byte(resp.MetadataJson), &meta); err == nil {
			for _, ss := range meta.SubServers {
				if ss.Name == subName {
					// A NeoForge build is recorded as its loader, as a setup does.
					return ss.Type, services.RecordedMinecraftVersion(ss.Type, ss.MinecraftVersion, ss.Build, ss.Build), ss.Build, true
				}
			}
		}
	}
	// Fall back to the directory scan when metadata is absent or omits this
	// sub-server. SubServerInfo carries no minecraft_version/build, so those
	// stay empty.
	for _, ss := range resp.SubServers {
		if ss.Name == subName {
			return ss.Type, "", "", true
		}
	}
	return "", "", "", false
}

func IsBYONNode(n models.Node) bool { return n.Kind() == models.NodeKindBYON }

// DylarisMetadata mirrors the relevant fields of .dylaris.json on disk.
type DylarisMetadata struct {
	Name            string  `json:"name"`
	MemoryMB        int     `json:"memory_mb"`
	CPULimit        float64 `json:"cpu_limit"`
	ActiveSubServer string  `json:"active_sub_server"`
	SubServers      []struct {
		Name             string `json:"name"`
		Type             string `json:"type"`
		MinecraftVersion string `json:"minecraft_version"`
		Build            string `json:"build"`
	} `json:"sub_servers"`
}
