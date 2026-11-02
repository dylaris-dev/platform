package files

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
)

// Every write names its paths in the body, and each handler refuses one that
// climbs out of the server directory before a node is asked. The node checks
// too; this is the half Core owns, one row per guard, so dropping any single
// check fails here. Copy's source used to have no check at all.
func TestEveryFileWriteRefusesAPathOutOfTheServer(t *testing.T) {
	rdb, _ := handlertest.Redis(t)
	h := NewFileHandler(quotaHTTPState(rdb))

	for name, tc := range map[string]struct {
		call func(http.ResponseWriter, *http.Request)
		body string
	}{
		"save":        {h.SaveFileHandler, `{"path":"../a.txt","content":"x"}`},
		"create":      {h.CreateFileHandler, `{"path":"../a.txt"}`},
		"rename":      {h.RenameFileHandler, `{"oldPath":"../a.txt","newPath":"b.txt"}`},
		"copy source": {h.CopyFileHandler, `{"oldPath":"../a.txt","newPath":"b.txt"}`},
		"copy target": {h.CopyFileHandler, `{"oldPath":"a.txt","newPath":"../b.txt"}`},
		"delete":      {h.DeleteFileHandler, `{"path":"../a.txt"}`},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/files/x?server_uuid=s1", strings.NewReader(tc.body))
			ctx := context.WithValue(req.Context(), "username", "u")
			ctx = context.WithValue(ctx, "isAdmin", true)
			rw := httptest.NewRecorder()
			tc.call(rw, req.WithContext(ctx))
			if rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), "nvalid") {
				t.Fatalf("status %d (%s), want the path refusal", rw.Code, rw.Body)
			}
		})
	}
}
