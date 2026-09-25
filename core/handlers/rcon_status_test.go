package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every RCON-backed endpoint answered 200 and put the failure in the body, so
// nothing outside the panel could tell a command that ran from one that never
// reached the server.
//
// Measured on production: a ban refused with "rcon not enabled for this server"
// answered 200 - and the server audit trail, which records a request that
// SUCCEEDED, then recorded the ban as having happened.
func TestWriteRconResponseStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp rconResponse
		want int
		why  string
	}{
		{"a command that ran", rconResponse{Success: true, Output: "There are 0 players"}, http.StatusOK,
			"the ordinary path must not move"},
		{"rcon switched off", rconResponse{Error: "rcon not enabled for this server", status: http.StatusConflict}, http.StatusConflict,
			"the server is not in a state where this can be done"},
		{"caller sent nothing", rconResponse{Error: "command required", status: http.StatusBadRequest}, http.StatusBadRequest,
			"the caller's fault"},
		{"node unreachable", rconResponse{Error: "the node did not answer"}, http.StatusBadGateway,
			"a failure with no status of its own left here and did not come back"},
		{"registry down", rconResponse{Error: "node registry not available", status: http.StatusServiceUnavailable}, http.StatusServiceUnavailable,
			"ours, and temporary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeRconResponse(rec, tc.resp)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, tc.why)
			}
			// The body shape is unchanged: the panel and the external API both
			// read success/error out of it, and status is unexported so it
			// cannot leak into the JSON.
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if _, ok := body["status"]; ok {
				t.Error("status reached the response body")
			}
			if body["success"] != tc.resp.Success {
				t.Errorf("success = %v, want %v", body["success"], tc.resp.Success)
			}
			if tc.resp.Error != "" && body["error"] != tc.resp.Error {
				t.Errorf("error = %v, want %q", body["error"], tc.resp.Error)
			}
		})
	}
}
