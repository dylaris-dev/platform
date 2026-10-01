package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-pkg/validate"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type ConsoleHandler struct {
	state *AppState
}

// consoleStreamKey resolves which log stream a console read should follow.
//
// The log shipper writes dylaris:server:<uuid>:logs:<sub> whenever the server
// has a sub-server, and every real server has one. An explicit ?sub_server=
// therefore wins, but an ABSENT one used to fall through to the un-suffixed
// key, which for such a server does not exist - so the request answered 200
// with an empty list while the server was running and its stream held
// hundreds of lines.
//
// The write side never had that problem: SendCommand pushes to
// dylaris:server:<uuid>:input, which is not per-sub-server, because only one
// sub-server runs at a time. So a caller could send commands to a server and
// hear nothing back, with neither call reporting an error. The read side now
// resolves the same "whichever one is running" the write side already assumes.
//
// The panel is unaffected: ConsoleView already passes the active sub-server
// on every request. This is for anyone who does not - the /api/external
// console route above all, where the caller is an integrator reading API.md
// and not a page that happens to hold the server object.
func consoleStreamKey(srv *models.Server, subServer string) string {
	if subServer == "" {
		subServer = srv.ActiveSubServer
	}
	if subServer == "" {
		return fmt.Sprintf("dylaris:server:%s:logs", srv.UUID)
	}
	return fmt.Sprintf("dylaris:server:%s:logs:%s", srv.UUID, subServer)
}

func NewConsoleHandler(state *AppState) *ConsoleHandler {
	return &ConsoleHandler{state: state}
}

// GetHistory GET /api/servers/{id}/console/history
// Returns the last 1000 log lines from the Redis Stream for this server.
func (h *ConsoleHandler) GetHistory(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil || h.state.Redis == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}

	subServer := r.URL.Query().Get("sub_server")
	if subServer != "" && !validate.IsSubServerName(subServer) {
		sendJSONError(w, "Invalid sub_server", http.StatusBadRequest)
		return
	}
	streamKey := consoleStreamKey(srv, subServer)
	// XRevRangeN returns newest-first; we reverse to get chronological order
	entries, err := h.state.Redis.XRevRangeN(r.Context(), streamKey, "+", "-", 1000).Result()
	if err != nil {
		// Stream may not exist yet (server never started) — return empty list
		entries = nil
	}

	lines := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		if v, ok := entries[i].Values["line"]; ok {
			lines = append(lines, fmt.Sprintf("%v", v))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"lines": lines})
}

// StreamConsole GET /api/servers/{id}/console/stream
// Streams live server logs via SSE (Server-Sent Events).
// Auth token can be passed as ?token= query param since EventSource
// does not support custom headers.
// Reads directly from the Redis Stream via XREAD BLOCK — no Pub/Sub,
// no watcher tracking needed. Delivery is reliable: messages are never
// lost between reconnects because they stay in the stream history.
func (h *ConsoleHandler) StreamConsole(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil || h.state.Redis == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		sendJSONError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	subServer := r.URL.Query().Get("sub_server")
	if subServer != "" && !validate.IsSubServerName(subServer) {
		sendJSONError(w, "Invalid sub_server", http.StatusBadRequest)
		return
	}
	streamKey := consoleStreamKey(srv, subServer)
	// "$" means: only deliver messages that arrive after this connection opens.
	// A reconnect names the last line it got (each line goes out with its
	// stream ID) and resumes right after it, so ending streams on a timer
	// loses nothing.
	lastID := "$"
	if id := r.Header.Get("Last-Event-ID"); sseEventID.MatchString(id) {
		lastID = id
	}

	ctx, cancel := streamContext(r)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// A blocking read does not notice the deadline, so it never blocks
		// past it.
		block := 5 * time.Second
		if dl, ok := ctx.Deadline(); ok {
			if rem := time.Until(dl); rem < block {
				block = rem
			}
		}
		// Redis counts BLOCK in milliseconds and reads 0 as "forever".
		if block < time.Millisecond {
			return
		}
		results, err := h.state.Redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, lastID},
			Count:   100,
			Block:   block,
		}).Result()
		if err == redis.Nil {
			// Block timeout with no new entries — send an SSE comment as keepalive
			// so the browser doesn't close the connection.
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
			continue
		}
		if err != nil {
			// r.Context() cancelled (client disconnect) or Redis error
			return
		}

		for _, stream := range results {
			for _, msg := range stream.Messages {
				line, _ := msg.Values["line"].(string)
				fmt.Fprintf(w, "id: %s\ndata: %s\n\n", msg.ID, sseEscape(line))
				flusher.Flush()
				lastID = msg.ID
			}
		}
	}
}

// SendCommand POST /api/servers/{id}/console/command - pushes one line onto
// the server's Redis input queue. Success means the node has been handed the
// command, not that the server has run it.
func (h *ConsoleHandler) SendCommand(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil || h.state.Redis == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Command string `json:"command"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, consoleCommandMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		sendJSONError(w, "Command required", http.StatusBadRequest)
		return
	}
	if msg := consoleCommandProblem(req.Command); msg != "" {
		sendJSONError(w, msg, http.StatusBadRequest)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	// Only a server that is running reads its input. A command sent to a
	// stopped one used to be accepted and wait in Redis for the next start -
	// a "stop" typed into a stopped server's console shut it down again the
	// moment it next came up - and nothing bounded how much could wait there,
	// in the Redis every tenant shares.
	if !consoleAcceptsInput(srv.Status) {
		sendJSONError(w, "Server is not running", http.StatusConflict)
		return
	}

	queueKey := fmt.Sprintf("dylaris:server:%s:input", srv.UUID)
	pipe := h.state.Redis.TxPipeline()
	pipe.RPush(r.Context(), queueKey, req.Command)
	// A running server drains this within a second; a backlog this long means
	// nothing is reading, and the newest commands are the ones to keep.
	pipe.LTrim(r.Context(), queueKey, -consoleInputBacklog, -1)
	if _, err := pipe.Exec(r.Context()); err != nil {
		sendJSONError(w, "Failed to send command", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

const (
	// consoleCommandMaxLen matches what the container's input forwarder keeps:
	// it cuts every line at 1 KB, so a longer one was never run as sent.
	consoleCommandMaxLen  = 1024
	consoleCommandMaxBody = 16 << 10
	consoleInputBacklog   = 100
)

// consoleCommandProblem refuses a command the server would not receive as
// typed. The forwarder strips line breaks; every other control character
// reached the server's stdin untouched.
func consoleCommandProblem(cmd string) string {
	if len(cmd) > consoleCommandMaxLen {
		return fmt.Sprintf("Command is too long (at most %d characters)", consoleCommandMaxLen)
	}
	for _, c := range cmd {
		if c < 0x20 || c == 0x7f {
			return "Command contains control characters"
		}
	}
	return ""
}

// consoleAcceptsInput reports whether a server in this status has a running
// container whose forwarder reads the input queue.
func consoleAcceptsInput(status string) bool {
	switch status {
	case "online", "starting", "restarting":
		return true
	}
	return false
}

// sseEscape prevents payload from breaking SSE framing.
// \r\n, \r, and \n are all line terminators in the SSE protocol and must be removed.
func sseEscape(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
