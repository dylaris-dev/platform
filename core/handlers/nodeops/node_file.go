package nodeops

import (
	"fmt"
	"time"

	pb "dylaris-proto/node"

	"github.com/google/uuid"

	nodegrpc "dylaris-core/grpc"
)

// ServerPropertiesName is the MC config file, relative to the active
// sub-server directory (the node joins it onto <storage>/<uuid>/<sub>/).
const ServerPropertiesName = "server.properties"

// ReadNodeFileString reads a whole (small) file from the node over the gRPC
// file API and returns its content. found=false with a nil error means the node
// reported the file does not exist (code 404); any other failure is a real
// error. Mirrors the streaming-read collection in FileHandler.GetFileContentHandler.
func ReadNodeFileString(reg *nodegrpc.Registry, nodeID int, serverUUID, relPath string) (string, bool, error) {
	if reg == nil {
		return "", false, fmt.Errorf("node registry not available")
	}
	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: relPath, ZipIfDir: false}},
	}
	ch, err := reg.SendRequestStreaming(nodeID, msg)
	if err != nil {
		return "", false, fmt.Errorf("node read failed: %w", err)
	}
	defer reg.CleanupRequest(nodeID, reqID)

	// server.properties is a few KB; the bound only stops a path that is not.
	data, errResp, rerr := CollectNodeFile(ch, 1<<20)
	if errResp != nil {
		if errResp.Code == 404 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("node read error: %s", errResp.Message)
	}
	if rerr != nil {
		return "", false, rerr
	}
	return string(data), true, nil
}

// WriteNodeFileString writes content to a node file over the gRPC file API
// (WriteFileReq -> DataChunk* -> TransferDone). Mirrors FileHandler.SaveFileHandler.
func WriteNodeFileString(reg *nodegrpc.Registry, nodeID int, serverUUID, relPath, content string) error {
	if reg == nil {
		return fmt.Errorf("node registry not available")
	}
	conn, ok := reg.GetConnection(nodeID)
	if !ok {
		return fmt.Errorf("node %d not connected", nodeID)
	}

	reqID := uuid.NewString()
	data := []byte(content)

	writeReqMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_WriteReq{WriteReq: &pb.WriteFileReq{Path: relPath, TotalSize: int64(len(data))}},
	}
	resp, err := reg.SendRequest(nodeID, writeReqMsg, 30*time.Second)
	if err != nil {
		return fmt.Errorf("node write init failed: %w", err)
	}
	if errResp := resp.GetError(); errResp != nil {
		return fmt.Errorf("node write error: %s", errResp.Message)
	}
	wait, stop := conn.AwaitReply(reqID)
	defer stop()

	const chunkSize = 64 * 1024
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunkMsg := &pb.NodeMessage{
			RequestId:  reqID,
			ServerUuid: serverUUID,
			Payload:    &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: data[offset:end], Offset: int64(offset)}},
		}
		if err := conn.Send(chunkMsg); err != nil {
			return fmt.Errorf("node write chunk failed: %w", err)
		}
	}

	doneMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: int64(len(data))}},
	}
	if err := conn.Send(doneMsg); err != nil {
		return fmt.Errorf("node write finalize failed: %w", err)
	}
	if _, msg, ok := ConfirmNodeWrite(wait); !ok {
		return fmt.Errorf("node write failed: %s", msg)
	}
	return nil
}
