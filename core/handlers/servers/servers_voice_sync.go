package servers

import (
	"context"
	"log"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
)

// syncVoiceToSubServer re-asserts a server's voice port into the voice mod
// config of the sub-server about to run, and refreshes the mapping. A server
// without a voice row is left alone: no node call, no write.
//
// Never blocks a start and never touches the RCON restart flag: every failure
// is logged and reported through the bool only.
func (h *ServerHandler) syncVoiceToSubServer(srv *models.Server, subServer string) bool {
	if subServer == "" {
		return true
	}
	if h.state == nil || h.state.Store == nil {
		return false
	}
	vp, err := h.state.Store.GetVoicePort(srv.ID)
	if err != nil {
		log.Printf("voice sync for server %d: reading the voice port failed: %v", srv.ID, err)
		return false
	}
	if vp == nil {
		return true
	}

	rh := handlers.NewRconHandler(h.state)
	mod, rel, content, err := rh.FindVoiceConfig(srv.NodeID, srv.UUID, subServer)
	if err != nil {
		log.Printf("voice sync for server %d (%s): %v", srv.ID, subServer, err)
		return false
	}
	var merged string
	switch mod {
	case "plasmo":
		merged = handlers.PlasmoVoiceEnable(content, vp.Host, vp.Port)
	default:
		merged = handlers.SvcVoiceEnable(content, vp.Host, vp.Port)
	}
	if merged != content {
		if err := rh.WriteNodeFileString(srv.NodeID, srv.UUID, subServer+"/"+rel, merged); err != nil {
			log.Printf("voice sync for server %d (%s): writing %s failed: %v", srv.ID, subServer, rel, err)
			return false
		}
	}

	// Update only the row read above, never AllocateVoicePort: that one
	// inserts, so a disable or region move landing between the read and here
	// would be undone - the row resurrected on a new port, or moved back.
	ok, err := h.state.Store.UpdateVoicePortMod(srv.ID, vp.Region, vp.Port, mod, services.VoiceInternalPort, vp.Host)
	if err != nil {
		log.Printf("voice sync for server %d: updating the voice port failed: %v", srv.ID, err)
		return false
	}
	if !ok {
		log.Printf("voice: server %d changed underneath, skipped", srv.ID)
		return false
	}
	got := *vp
	got.Mod, got.InternalPort = mod, services.VoiceInternalPort
	if h.state.Redis == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := services.PublishVoice(ctx, h.state.Redis, got); err != nil {
		log.Printf("voice sync for server %d: %v", srv.ID, err)
		return false
	}
	return true
}
