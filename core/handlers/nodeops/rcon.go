package nodeops

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	pb "dylaris-proto/node"

	"github.com/google/uuid"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/store"
)

const (
	// rconExecTimeout is how long we wait for the node to come back with a
	// reply. MC servers typically reply instantly but a frozen server can
	// hang the TCP socket — we'd rather report timeout to the user than
	// stall a request thread.
	rconExecTimeout   = 5 * time.Second
	rconMaxCommandLen = 1024
	DefaultRconPort   = 25575

	// CommandMaxBody bounds a console or RCON command request body.
	CommandMaxBody = 16 << 10
)

type RconRequest struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
}

type RconResponse struct {
	Success    bool   `json:"success"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`

	// Status is the HTTP status WriteRconResponse sends. Tagged "-", so it is
	// never part of the body and the wire shape is unchanged.
	//
	// It exists because every one of these endpoints answered 200 and put the
	// failure in the body, so nothing outside the panel could tell a ban that
	// happened from one that never reached the server. Measured on production:
	// a ban refused with "rcon not enabled" answered 200, and the server audit
	// trail - which records a successful request - then recorded the ban.
	Status int `json:"-"`
}

// WriteRconResponse is the single writer for all four RCON-backed endpoints.
// A failure with no status of its own is a bad gateway: the command left here
// and the answer did not come back.
func WriteRconResponse(w http.ResponseWriter, resp RconResponse) {
	code := resp.Status
	if code == 0 {
		if resp.Success {
			code = http.StatusOK
		} else {
			code = http.StatusBadGateway
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// ExecRcon runs one RCON command on a server through its node.
func ExecRcon(ctx context.Context, st store.Store, reg *nodegrpc.Registry, serverID int, serverUUID string, nodeID int, req RconRequest) RconResponse {
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		return RconResponse{Error: "command required", Status: http.StatusBadRequest}
	}
	if len(req.Command) > rconMaxCommandLen {
		return RconResponse{Error: "command too long", Status: http.StatusBadRequest}
	}
	// The same characters the console refuses.
	for _, c := range req.Command {
		if c < 0x20 || c == 0x7f {
			return RconResponse{Error: "command contains control characters", Status: http.StatusBadRequest}
		}
	}

	enabled, port, password, err := st.GetServerRconConfig(serverID)
	if err != nil {
		return RconResponse{Error: "failed to load rcon config", Status: http.StatusInternalServerError}
	}
	if !enabled || password == "" {
		return RconResponse{Error: "rcon not enabled for this server", Status: http.StatusConflict}
	}
	if port == 0 {
		port = DefaultRconPort
	}
	if reg == nil {
		return RconResponse{Error: "node registry not available", Status: http.StatusServiceUnavailable}
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 || timeoutMs > 10_000 {
		timeoutMs = int(rconExecTimeout / time.Millisecond)
	}

	msg := &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_RconExecReq{
			RconExecReq: &pb.RconExecReq{
				Command:      req.Command,
				RconPassword: password,
				RconPort:     int32(port),
				TimeoutMs:    int32(timeoutMs),
			},
		},
	}
	respMsg, err := reg.SendRequest(nodeID, msg, time.Duration(timeoutMs+1000)*time.Millisecond)
	if err != nil {
		return RconResponse{Error: rconFailureMessage(serverUUID, nodeID, err.Error())}
	}
	rconResp := respMsg.GetRconExecResp()
	if rconResp == nil {
		if errMsg := respMsg.GetError(); errMsg != nil {
			return RconResponse{Error: rconFailureMessage(serverUUID, nodeID, errMsg.Message)}
		}
		return RconResponse{Error: "unexpected response from node", Status: http.StatusBadGateway}
	}
	if rconResp.Error != "" {
		return RconResponse{
			Success:    rconResp.Ok,
			Output:     rconResp.Output,
			Error:      rconFailureMessage(serverUUID, nodeID, rconResp.Error),
			DurationMs: rconResp.DurationMs,
		}
	}
	return RconResponse{
		Success:    rconResp.Ok,
		Output:     rconResp.Output,
		DurationMs: rconResp.DurationMs,
	}
}

// rconFailureMessage logs what actually went wrong and returns what the caller
// is told. The two are not the same thing: the node's error is a dial error, so
// it carries the container's private overlay address, and RCON is handed out as
// its own capability - a friend granted rcon.exec holds neither network.read
// nor any other right to the topology. Measured on production: a delegate
// running one command read back
// "dial 10.20.13.16:25575: ... connect: connection refused".
//
// The one case worth naming is a refused connection, because it has an action
// attached and is what an operator hits right after enabling RCON: MC only
// opens the listener at JVM start. Everything else is one message plus a log
// line, the same trade the tab proxy makes.
func rconFailureMessage(serverUUID string, nodeID int, detail string) string {
	log.Printf("rcon: server %s on node %d failed: %s", serverUUID, nodeID, detail)
	if strings.Contains(detail, "connection refused") {
		return "The server is not accepting RCON connections. If RCON was just enabled, restart the server to apply it."
	}
	if strings.Contains(detail, "timeout") || strings.Contains(detail, "deadline exceeded") {
		return "The server did not answer the RCON command in time."
	}
	if strings.Contains(detail, "authentication") || strings.Contains(detail, "password") {
		return "The server refused the RCON password. Regenerate it in the RCON settings and restart the server."
	}
	return "The RCON command could not be delivered to the server."
}
