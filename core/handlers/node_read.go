package handlers

import (
	"bytes"
	"errors"
	"net/http"

	nodegrpc "dylaris-core/grpc"
	pb "dylaris-proto/node"
)

// maxOpenFileBytes is the most a file may hold to be opened as text. Opening
// collects the whole file in Core's memory, and there was no limit at all: a
// user with read access to one server's files could open a region file or a
// backup of several gigabytes and take Core down for every tenant. A file this
// large is not one anybody edits in a browser; it is downloaded.
const maxOpenFileBytes = 10 << 20

var errFileTooLargeToOpen = errors.New("this file is too large to open here (over 10 MB); download it instead")

// errTransferIncomplete is a stream that ended without the node's final
// TransferDone: the node went away, or the read stalled and was ended. What
// arrived is a PART of the file, and an editor that saved it would cut it.
var errTransferIncomplete = errors.New("the node stopped sending before the file was complete; try again")

// readErrStatus is the HTTP status for an error from collectNodeFile.
func readErrStatus(err error) int {
	if errors.Is(err, errFileTooLargeToOpen) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadGateway
}

// collectNodeFile gathers a streamed node read into memory, up to max bytes.
// A node error comes back as the second result; going over max is
// errFileTooLargeToOpen. The caller's CleanupRequest stops the stream.
func collectNodeFile(ch <-chan *pb.NodeMessage, max int) ([]byte, *pb.OpError, error) {
	var buf bytes.Buffer
	complete := false
	for resp := range ch {
		if e := resp.GetError(); e != nil {
			return nil, e, nil
		}
		if nodegrpc.IsFinalTransferDone(resp) {
			complete = true
		}
		if chunk := resp.GetChunk(); chunk != nil {
			if buf.Len()+len(chunk.Data) > max {
				return nil, nil, errFileTooLargeToOpen
			}
			buf.Write(chunk.Data)
		}
	}
	if !complete {
		return nil, nil, errTransferIncomplete
	}
	return buf.Bytes(), nil, nil
}

// nodeErrorStatus is the HTTP status Core answers with for a node's error.
//
// The node's code was passed through as it came. A node can be a customer's
// own machine, and a 401 from it is what the panel reads as "your session
// ended": everyone browsing files on that node was signed out on every
// attempt. A code outside the HTTP range made WriteHeader panic. The codes a
// file operation honestly produces pass; anything else is a bad gateway.
func nodeErrorStatus(code int32) int {
	switch c := int(code); c {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusNotImplemented, http.StatusServiceUnavailable,
		http.StatusInsufficientStorage:
		return c
	}
	return http.StatusBadGateway
}

// refuseIncomplete ends a download whose stream closed without the node's
// final TransferDone. Before the body started that is an honest 502; after it,
// the connection is aborted, so the browser reports a failed download instead
// of saving a truncated file as a finished one.
func refuseIncomplete(w http.ResponseWriter, complete, bodyStarted bool) {
	if complete {
		return
	}
	if !bodyStarted {
		w.Header().Del("Content-Disposition")
		http.Error(w, errTransferIncomplete.Error(), http.StatusBadGateway)
		return
	}
	panic(http.ErrAbortHandler)
}
