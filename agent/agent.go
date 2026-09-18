package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

type HistoryPoint struct {
	Timestamp int64   `json:"ts"`
	CPU       float64 `json:"cpu"`
	RAM       float64 `json:"ram"`
	Rx        uint64  `json:"rx"`
	Tx        uint64  `json:"tx"`
}

type PersistentData struct {
	TotalRxOffset uint64 `json:"rx_offset"`
	TotalTxOffset uint64 `json:"tx_offset"`

	History        []HistoryPoint `json:"history"`
	AutoResetDay   int            `json:"auto_reset_day"`
	LastResetMonth string         `json:"last_reset_month"`
}

type SystemStats struct {
	CPUPercent float64 `json:"cpu_percent"`
	RAMUsed    uint64  `json:"ram_used"`
	RAMTotal   uint64  `json:"ram_total"`
	RAMPercent float64 `json:"ram_percent"`
	NetRxSpeed uint64  `json:"net_rx_speed"`
	NetTxSpeed uint64  `json:"net_tx_speed"`
	NetTotalRx uint64  `json:"net_total_rx"`
	NetTotalTx uint64  `json:"net_total_tx"`
	// History is omitempty: stat snapshots skip it to keep the Redis/bandwidth payload small.
	History      []HistoryPoint `json:"history,omitempty"`
	AutoResetDay int            `json:"auto_reset_day"`
}

// SystemSnapshot is a flat, JSON-serializable struct for easy Redis Stream publishing.
type SystemSnapshot struct {
	Timestamp  int64   `json:"ts"`
	CPUPercent float64 `json:"cpu"`
	RAMUsed    uint64  `json:"ram_used"`
	RAMTotal   uint64  `json:"ram_total"`
	RAMPercent float64 `json:"ram_pct"`
	NetRxSpeed uint64  `json:"rx_speed"`
	NetTxSpeed uint64  `json:"tx_speed"`
	NetTotalRx uint64  `json:"rx_total"`
	NetTotalTx uint64  `json:"tx_total"`
}

// MonitorConfig controls which optional components are initialized.
type MonitorConfig struct {
	DataFile string // Path for persistent history JSON. Empty = no persistence.
}

type Monitor struct {
	mu           sync.Mutex
	lastNetStats []net.IOCountersStat
	lastTime     time.Time
	dataFile     string
	data         PersistentData

	bootRx uint64
	bootTx uint64

	// Cached delta-based stats, updated by GetCurrentStats (background poller).
	// Snapshot() reads these instead of calling cpu.Percent again, which would
	// return ~0% when called milliseconds after the background poller.
	cachedCPU     float64
	cachedRxSpeed uint64
	cachedTxSpeed uint64
}

func NewMonitor(cfg MonitorConfig) (*Monitor, error) {
	netStats, err := net.IOCounters(false)
	if err != nil {
		return nil, err
	}
	// Guard the [0] indexing below: gopsutil can return an empty aggregate on
	// hosts/containers with no usable interface. Fail cleanly instead of panicking.
	if len(netStats) == 0 {
		return nil, fmt.Errorf("no network interfaces reported")
	}

	m := &Monitor{
		lastNetStats: netStats,
		lastTime:     time.Now(),
		dataFile:     cfg.DataFile,
		bootRx:       netStats[0].BytesRecv,
		bootTx:       netStats[0].BytesSent,
	}

	if cfg.DataFile != "" {
		m.loadData()
	}

	if m.data.History == nil {
		m.data.History = make([]HistoryPoint, 0)
	}

	return m, nil
}

// Snapshot returns a flat struct with current system metrics, suitable for Redis publishing.
// It uses cached CPU and network speed values set by the most recent GetCurrentStats call
// (background poller) to avoid a race condition where multiple goroutines call cpu.Percent(0)
// within milliseconds of each other, causing near-zero measurements.
func (m *Monitor) Snapshot() (*SystemSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vMem, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	var totalRx, totalTx uint64
	currentNetStats, err := net.IOCounters(false)
	if err == nil && len(currentNetStats) > 0 {
		current := currentNetStats[0]
		currentSessionRx := subClamp(current.BytesRecv, m.bootRx)
		currentSessionTx := subClamp(current.BytesSent, m.bootTx)
		totalRx = currentSessionRx + m.data.TotalRxOffset
		totalTx = currentSessionTx + m.data.TotalTxOffset
	}

	return &SystemSnapshot{
		Timestamp:  time.Now().Unix(),
		CPUPercent: m.cachedCPU,
		RAMUsed:    vMem.Used,
		RAMTotal:   vMem.Total,
		RAMPercent: vMem.UsedPercent,
		NetRxSpeed: m.cachedRxSpeed,
		NetTxSpeed: m.cachedTxSpeed,
		NetTotalRx: totalRx,
		NetTotalTx: totalTx,
	}, nil
}

// subClamp returns a-b, or 0 when b > a. Network byte counters reset on an
// interface/host reboot; without clamping the unsigned subtraction underflows
// into a bogus huge value that then gets pushed to Redis.
func subClamp(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// GetCurrentStats returns ONLY the live data (CPU, RAM, Speed)
func (m *Monitor) GetCurrentStats() (*SystemStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := &SystemStats{
		AutoResetDay: m.data.AutoResetDay,
	}

	// RAM
	vMem, err := mem.VirtualMemory()
	if err == nil {
		stats.RAMUsed = vMem.Used
		stats.RAMTotal = vMem.Total
		stats.RAMPercent = vMem.UsedPercent
	}

	// CPU
	cpuP, err := cpu.Percent(0, false)
	if err == nil && len(cpuP) > 0 {
		stats.CPUPercent = cpuP[0]
		m.cachedCPU = cpuP[0]
	}

	// NET
	currentNetStats, err := net.IOCounters(false)
	if err == nil && len(currentNetStats) > 0 {
		current := currentNetStats[0]
		prev := m.lastNetStats[0]
		now := time.Now()
		duration := now.Sub(m.lastTime).Seconds()

		if duration > 0 {
			rxDiff := float64(subClamp(current.BytesRecv, prev.BytesRecv))
			txDiff := float64(subClamp(current.BytesSent, prev.BytesSent))
			stats.NetRxSpeed = uint64(rxDiff / duration)
			stats.NetTxSpeed = uint64(txDiff / duration)
			m.cachedRxSpeed = stats.NetRxSpeed
			m.cachedTxSpeed = stats.NetTxSpeed
		}

		currentSessionRx := subClamp(current.BytesRecv, m.bootRx)
		currentSessionTx := subClamp(current.BytesSent, m.bootTx)

		stats.NetTotalRx = currentSessionRx + m.data.TotalRxOffset
		stats.NetTotalTx = currentSessionTx + m.data.TotalTxOffset

		m.lastNetStats = currentNetStats
		m.lastTime = now
	}

	return stats, nil
}

// GetSmartHistory returns a downsampled version of the history based on the requested time range.
// It ensures we never send too many points to the frontend.
func (m *Monitor) GetSmartHistory(rangeSeconds int64) []HistoryPoint {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.data.History) == 0 {
		return []HistoryPoint{}
	}

	cutoff := time.Now().Unix() - rangeSeconds

	// 1. Filter: Only take points within the requested range
	var filtered []HistoryPoint
	for _, p := range m.data.History {
		if p.Timestamp >= cutoff {
			filtered = append(filtered, p)
		}
	}

	count := len(filtered)
	if count == 0 {
		return []HistoryPoint{}
	}

	// 2. Downsample: If we have too many points, average them.
	// Cap at 300 points: chart render/payload budget for the frontend.
	maxPoints := 300
	if count <= maxPoints {
		return filtered
	}

	step := count / maxPoints
	var result []HistoryPoint

	for i := 0; i < count; i += step {
		end := i + step
		if end > count {
			end = count
		}

		var sumCPU, sumRAM float64
		var sumRx, sumTx uint64

		chunkSize := float64(end - i)

		for j := i; j < end; j++ {
			sumCPU += filtered[j].CPU
			sumRAM += filtered[j].RAM
			sumRx += filtered[j].Rx
			sumTx += filtered[j].Tx
		}

		p := HistoryPoint{
			Timestamp: filtered[i].Timestamp, // Use timestamp of first point in chunk
			CPU:       sumCPU / chunkSize,
			RAM:       sumRAM / chunkSize,
			Rx:        uint64(float64(sumRx) / chunkSize),
			Tx:        uint64(float64(sumTx) / chunkSize),
		}
		result = append(result, p)
	}

	return result
}

// --- STANDARD METHODS ---

func (m *Monitor) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentNetStats, err := net.IOCounters(false)
	if err == nil && len(currentNetStats) > 0 {
		current := currentNetStats[0]
		sessionRx := subClamp(current.BytesRecv, m.bootRx)
		sessionTx := subClamp(current.BytesSent, m.bootTx)
		m.data.TotalRxOffset += sessionRx
		m.data.TotalTxOffset += sessionTx
		m.bootRx = current.BytesRecv
		m.bootTx = current.BytesSent
		m.saveDataLocked()
	}
}

func (m *Monitor) RecordHistoryPoint() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cpuP, _ := cpu.Percent(0, false)
	vMem, _ := mem.VirtualMemory()

	c := 0.0
	if len(cpuP) > 0 {
		c = cpuP[0]
	}
	r := 0.0
	if vMem != nil {
		r = vMem.UsedPercent
	}

	point := HistoryPoint{
		Timestamp: time.Now().Unix(),
		CPU:       c,
		RAM:       r,
		Rx:        0,
		Tx:        0,
	}

	// Keep 30 days of raw data (1 point per min = ~43200 points)
	maxPoints := 45000
	m.data.History = append(m.data.History, point)
	if len(m.data.History) > maxPoints {
		startIdx := len(m.data.History) - maxPoints
		m.data.History = m.data.History[startIdx:]
	}

	m.saveDataLocked()
}

func (m *Monitor) UpdateLastHistoryPoint(rxSpeed, txSpeed uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.data.History) == 0 {
		return
	}
	idx := len(m.data.History) - 1
	m.data.History[idx].Rx = rxSpeed
	m.data.History[idx].Tx = txSpeed
}

func (m *Monitor) CheckAutoReset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.AutoResetDay == 0 {
		return
	}
	now := time.Now()
	if now.Day() == m.data.AutoResetDay {
		currentMonthStr := now.Format("2006-01")
		if m.data.LastResetMonth != currentMonthStr {
			m.data.TotalRxOffset = 0
			m.data.TotalTxOffset = 0
			m.bootRx = m.lastNetStats[0].BytesRecv
			m.bootTx = m.lastNetStats[0].BytesSent
			m.data.LastResetMonth = currentMonthStr
			m.saveDataLocked()
		}
	}
}

func (m *Monitor) SetAutoResetDay(day int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.AutoResetDay = day
	m.saveDataLocked()
}

func (m *Monitor) ResetTraffic() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.TotalRxOffset = 0
	m.data.TotalTxOffset = 0
	m.bootRx = m.lastNetStats[0].BytesRecv
	m.bootTx = m.lastNetStats[0].BytesSent
	m.saveDataLocked()
}

func (m *Monitor) ResetHistory() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.History = []HistoryPoint{}
	m.saveDataLocked()
}

func (m *Monitor) ResetAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.TotalRxOffset = 0
	m.data.TotalTxOffset = 0
	m.bootRx = m.lastNetStats[0].BytesRecv
	m.bootTx = m.lastNetStats[0].BytesSent
	m.data.History = []HistoryPoint{}
	m.saveDataLocked()
}

func (m *Monitor) loadData() {
	file, err := os.ReadFile(m.dataFile)
	if err != nil {
		return
	}
	// Log a corrupt/truncated data file instead of silently resetting all
	// traffic offsets and history to zero with no signal.
	if err := json.Unmarshal(file, &m.data); err != nil {
		log.Printf("agent: corrupt monitor data file %q, ignoring: %v", m.dataFile, err)
	}
}

func (m *Monitor) saveDataLocked() {
	if m.dataFile == "" {
		return
	}
	data, err := json.Marshal(m.data)
	if err != nil {
		log.Printf("agent: marshal monitor data: %v", err)
		return
	}
	// Atomic write: stage to a temp file then rename over the target so a crash
	// mid-write can't leave a truncated/corrupt data file.
	tmp := m.dataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("agent: write monitor data: %v", err)
		return
	}
	if err := os.Rename(tmp, m.dataFile); err != nil {
		log.Printf("agent: rename monitor data: %v", err)
		os.Remove(tmp)
	}
}
