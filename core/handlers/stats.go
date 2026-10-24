package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dylaris-core/services"

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
	release, ok := acquireLiveStream(w, r, h.state.Store, "stats", statsStreamsPerUser)
	if !ok {
		return
	}
	defer release()

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

	ctx, cancel := streamContext(w, r)
	defer cancel()

	// Say first whether the buffer describes a RUNNING server. The node stops
	// writing it when the container goes away but the stream stays for an
	// hour, and replaying it made a stopped server's last hour look current.
	// A named event, so a panel that does not listen for it ignores it.
	state := readStatsStreamState(ctx, h.state.Redis, bufferKey, srv.Status, time.Now())
	if b, err := json.Marshal(state); err == nil {
		fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
	}

	// Send buffered data first (Redis Stream via XRANGE) - on a first connect
	// only, and only while the server is live. The id below marks a stream
	// that has already done so, and the browser sends it back when it
	// reconnects: streams end on a timer now, and replaying the buffer every
	// time would draw it into the live graph again.
	if r.Header.Get("Last-Event-ID") == "" && state.Live {
		for _, data := range statsReplay(ctx, h.state.Redis, bufferKey) {
			fmt.Fprintf(w, "data: %s\n\n", data)
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

// statsStreamState is the `state` event of the stats stream.
type statsStreamState struct {
	Status string `json:"status"` // the server's status as Core has it
	// Live: the newest buffered sample is fresh, so the buffer describes the
	// server as it runs now. False means nothing it holds is current.
	Live bool `json:"live"`
	// LastSampleAt is the newest sample's time in unix ms, null when none.
	LastSampleAt *int64 `json:"lastSampleAt"`
}

func readStatsStreamState(ctx context.Context, rdb *redis.Client, bufferKey, status string, now time.Time) statsStreamState {
	st := statsStreamState{Status: status}
	msgs, err := rdb.XRevRangeN(ctx, bufferKey, "+", "-", 1).Result()
	if err != nil || len(msgs) == 0 {
		return st
	}
	if at, ok := services.StatsEntryTime(msgs[0].ID); ok {
		ms := at.UnixMilli()
		st.LastSampleAt = &ms
		st.Live = services.StatsSampleFresh(at, now)
	}
	return st
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
		// MB; null when the node reported no heap for that sample.
		JavaHeapUsed *int64 `json:"javaHeapUsed"`
	}

	points := make([]historyPoint, 0, len(rows))
	for _, row := range rows {
		points = append(points, historyPoint{
			TS:           row.Time.Unix(),
			CPU:          row.CPU,
			CPULimit:     row.CPULimit,
			MemUsed:      row.MemUsed,
			MemLimit:     row.MemLimit,
			Players:      row.Players,
			MaxPlayers:   row.MaxPlayers,
			JavaHeapUsed: row.JavaHeapUsed,
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

// coreLoadEntry is one pinned core in GET /stats/cores. ID and Shared are
// filled for an admin only: a member without the settings capability is not
// shown the server's cpuset elsewhere either, so they get a position, not an id.
type coreLoadEntry struct {
	Index  int     `json:"index"`
	ID     *int    `json:"id,omitempty"`
	Load   float64 `json:"load"`
	Shared bool    `json:"shared,omitempty"`
}

// GetCores GET /api/servers/{id}/stats/cores - how busy each core the server
// is pinned to is.
//
// The load is the HOST's per core (the node cannot measure a container per
// core), so it is only the server's own when no other server can run on those
// cores. A member sees it only then; an admin always, with the shared cores
// marked. An unpinned server, or a node too old to publish the load, answers
// with no cores.
func (h *StatsHandler) GetCores(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil || h.state.Redis == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	serverID, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	out := struct {
		Cores     []coreLoadEntry `json:"cores"`
		Exclusive bool            `json:"exclusive"`
	}{Cores: []coreLoadEntry{}}
	w.Header().Set("Content-Type", "application/json")

	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		json.NewEncoder(w).Encode(out)
		return
	}
	var published struct {
		Cores map[int]float64 `json:"cores"`
	}
	raw, err := h.state.Redis.Get(r.Context(), fmt.Sprintf("dylaris:node:%s:cpuload", node.Token)).Bytes()
	if err != nil || json.Unmarshal(raw, &published) != nil || len(published.Cores) == 0 {
		json.NewEncoder(w).Encode(out)
		return
	}
	others, err := h.state.Store.ListServerPinningByNode(node.ID)
	if err != nil {
		sendJSONError(w, "Could not read the node's servers", http.StatusInternalServerError)
		return
	}
	all := make([]int, 0, len(published.Cores))
	for c := range published.Cores {
		all = append(all, c)
	}
	cores, shared := services.PinnedCoreSharing(*srv, others, node.CpusetCpus, all)
	out.Exclusive = len(cores) > 0 && len(shared) == 0
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if isAdmin || out.Exclusive {
		for _, c := range cores {
			load, ok := published.Cores[c]
			if !ok {
				continue // a core the host does not have (or did not report)
			}
			// Counted over what is shown, so a skipped core leaves no gap.
			e := coreLoadEntry{Index: len(out.Cores) + 1, Load: load}
			if isAdmin {
				id := c
				e.ID, e.Shared = &id, shared[c]
			}
			out.Cores = append(out.Cores, e)
		}
	}
	json.NewEncoder(w).Encode(out)
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

// statsReplayMax is how many buffered samples a new stream starts with: what
// the live chart holds. The node keeps an hour (1800), and sending all of it
// made the panel take every sample as its own update only to keep the last 60.
const statsReplayMax = 60

// statsReplay returns the newest buffered samples, oldest first.
func statsReplay(ctx context.Context, rdb *redis.Client, bufferKey string) []string {
	entries, err := rdb.XRevRangeN(ctx, bufferKey, "+", "-", statsReplayMax).Result()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		if data, ok := entries[i].Values["data"].(string); ok {
			out = append(out, data)
		}
	}
	return out
}
