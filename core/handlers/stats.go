package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type StatsHandler struct {
	state *AppState
}

func NewStatsHandler(state *AppState) *StatsHandler {
	return &StatsHandler{state: state}
}

// StreamStats GET /api/servers/{id}/stats/stream
// SSE endpoint: sends buffered data from Redis Stream first, then live pub/sub updates.
// Sets a watching key so nodes only publish when someone is actually watching.
func (h *StatsHandler) StreamStats(w http.ResponseWriter, r *http.Request) {
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

	bufferKey := fmt.Sprintf("dylaris:server:%s:stats:buffer", srv.UUID)
	liveKey := fmt.Sprintf("dylaris:server:%s:stats:live", srv.UUID)
	watchKey := fmt.Sprintf("dylaris:server:%s:stats:watching", srv.UUID)

	// Set watching key so the node knows to publish live updates
	h.state.Redis.Set(r.Context(), watchKey, "1", 10*time.Second)
	defer clearStatsWatch(h.state.Redis, watchKey)

	// Refresh watching key in background
	watchDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-ticker.C:
				h.state.Redis.Set(r.Context(), watchKey, "1", 10*time.Second)
			}
		}
	}()
	defer close(watchDone)

	ctx, cancel := streamContext(r)
	defer cancel()

	// Send buffered data first (Redis Stream via XRANGE) - on a first connect
	// only. The id below marks a stream that has already done so, and the
	// browser sends it back when it reconnects: streams end on a timer now,
	// and replaying the buffer every time would draw it into the live graph
	// again.
	if r.Header.Get("Last-Event-ID") == "" {
		entries, err := h.state.Redis.XRange(ctx, bufferKey, "-", "+").Result()
		if err == nil {
			for _, msg := range entries {
				if data, ok := msg.Values["data"].(string); ok {
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
			}
		}
	}
	fmt.Fprintf(w, "id: live\n\n")
	flusher.Flush()

	// Subscribe to live updates
	pubsub := h.state.Redis.Subscribe(ctx, liveKey)
	defer pubsub.Close()

	ch := pubsub.Channel()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg.Payload)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// GetHistory GET /api/servers/{id}/stats/history?range=24h
// Returns historical stats data points from PostgreSQL.
func (h *StatsHandler) GetHistory(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
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

	// Parse time range (default: 24h)
	rangeStr := r.URL.Query().Get("range")
	duration := 24 * time.Hour
	switch rangeStr {
	case "1h":
		duration = 1 * time.Hour
	case "6h":
		duration = 6 * time.Hour
	case "12h":
		duration = 12 * time.Hour
	}

	since := time.Now().Add(-duration)
	rows, err := h.state.Store.GetStatsHistory(srv.UUID, since)
	if err != nil {
		rows = nil
	}

	type historyPoint struct {
		TS         int64   `json:"ts"`
		CPU        float64 `json:"cpu"`
		CPULimit   float64 `json:"cpuLimit"`
		MemUsed    int64   `json:"memUsed"`
		MemLimit   int64   `json:"memLimit"`
		Players    int     `json:"players"`
		MaxPlayers int     `json:"maxPlayers"`
	}

	points := make([]historyPoint, 0, len(rows))
	for _, row := range rows {
		points = append(points, historyPoint{
			TS:         row.Time.Unix(),
			CPU:        row.CPU,
			CPULimit:   row.CPULimit,
			MemUsed:    row.MemUsed,
			MemLimit:   row.MemLimit,
			Players:    row.Players,
			MaxPlayers: row.MaxPlayers,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"points": points})
}

// GetDisk GET /api/servers/{id}/stats/disk
// Returns disk usage data for the server.
func (h *StatsHandler) GetDisk(w http.ResponseWriter, r *http.Request) {
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

	diskKey := fmt.Sprintf("dylaris:server:%s:stats:disk", srv.UUID)
	data, err := h.state.Redis.Get(r.Context(), diskKey).Result()
	if err != nil {
		// No data yet
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total":      0,
			"subServers": map[string]int64{},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(data))
}

// clearStatsWatch removes the watch key so the node stops publishing live stats
// as soon as the last viewer leaves.
//
// It takes a DETACHED context on purpose. This runs from a deferred call in an
// SSE handler whose normal exit is `<-r.Context().Done()` - the client
// disconnecting IS how the handler ends - so by the time the cleanup runs the
// request context is already cancelled, go-redis checks ctx.Err() before it
// dispatches, and the Del never reaches Redis at all. The key then lingers for
// the rest of its 10s TTL and the node keeps publishing to nobody.
func clearStatsWatch(rdb *redis.Client, watchKey string) {
	if rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rdb.Del(ctx, watchKey)
}
