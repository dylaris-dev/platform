package services

import (
	"testing"
	"time"

	"dylaris-pkg/protocol"
)

// The edge publishes its own struct, not protocol.GatewayStats, and Core reads
// it with ParseGatewayStats. A "counters" map on that record is all an edge
// needs to add for its logins to be recorded as edge.logins - no Core change.
func TestAnEdgeLoginsCounterIsRecordedAsEdgeLogins(t *testing.T) {
	record := `{"v":1,"component":"edge","id":"edge-1","region":"eu","rx_speed":1,` +
		`"active_mc_streams":3,"gauges":{"players":3},"counters":{"logins":2}}`
	gs, ok, err := protocol.ParseGatewayStats([]byte(record))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}

	c, capt := newInfraCollector(t)
	c.SetGatewayStats(gwSnapshot{gs})
	c.sampleGatewayAll(time.Now())
	if err := c.recorders.Recorder().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := capt.one(t, "edge.logins")
	if row.Bucket.Sum != 2 || row.Key.Subject != "edge-1" || row.Key.Region != "eu" {
		t.Fatalf("edge.logins = %+v", row)
	}
}
