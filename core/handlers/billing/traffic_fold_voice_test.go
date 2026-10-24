package billing

import (
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/store"
)

// Voice rides in the player pool as a subset: the pool reports it, and its
// usedGb is the plain sum of bytes, exactly as before voice existed. Bytes are
// whole GB so a voice share added on top would move usedGb.
func TestFoldTrafficCellsCarriesVoice(t *testing.T) {
	fs := &voicePoolStore{cells: []store.RegionUsage{
		{Region: "eu", Kind: store.TrafficKindEdge, Product: store.TrafficProductBYON, Bytes: 1000 * trafficGB, VoiceBytes: 300 * trafficGB},
		{Region: "eu", Kind: store.TrafficKindEdge, Product: store.TrafficProductRoute, Bytes: 500 * trafficGB, VoiceBytes: 0},
	}}
	h := &BillingHandler{state: &handlers.AppState{Store: fs}}
	one := int64(1)
	st := h.trafficStatusFor("u1", &store.UserBilling{MaxNodes: &one})
	if st == nil {
		t.Fatal("no traffic status")
	}
	var edge *trafficPool
	for i := range st.Pools {
		if st.Pools[i].Kind == store.TrafficKindEdge {
			edge = &st.Pools[i]
		}
	}
	if edge == nil {
		t.Fatalf("no edge pool in %+v", st.Pools)
	}
	if edge.VoiceBytes != 300*trafficGB {
		t.Errorf("voiceBytes = %d, want %d", edge.VoiceBytes, 300*trafficGB)
	}
	if edge.UsedGB != 1500 {
		t.Errorf("usedGb = %d, want 1500 - voice must not be added on top", edge.UsedGB)
	}
}
