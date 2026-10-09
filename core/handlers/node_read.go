package handlers

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"

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

var errGzipTooLargeToOpen = errors.New("this file is over 10 MB once decompressed, too large to open here; download it instead")

var errGzipCorrupt = errors.New("this .gz file could not be decompressed; it is damaged or not gzip. Download it instead")

// readErrStatus is the HTTP status for an error from collectNodeFile or
// decodeOpenedFile.
func readErrStatus(err error) int {
	switch {
	case errors.Is(err, errFileTooLargeToOpen), errors.Is(err, errGzipTooLargeToOpen):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errGzipCorrupt):
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}

// decodeOpenedFile turns a collected file into what the editor shows. A .gz
// (rotated server logs are latest.log -> 2026-10-09-1.log.gz) is decompressed,
// bounded by max like any opened file so a small archive cannot inflate into
// gigabytes in Core's memory, and is readonly: saving the text back would
// replace the archive with plain text.
func decodeOpenedFile(path string, data []byte, max int) (content []byte, readonly bool, err error) {
	if !strings.HasSuffix(strings.ToLower(path), ".gz") {
		return data, false, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, true, errGzipCorrupt
	}
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		return nil, true, errGzipCorrupt
	}
	if len(out) > max {
		return nil, true, errGzipTooLargeToOpen
	}
	return out, true, nil
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
