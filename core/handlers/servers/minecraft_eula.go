package servers

import (
	"log"
	"net/http"

	"dylaris-core/handlers"
)

// minecraftEulaURL is where the panel sends people to read what they accept.
const minecraftEulaURL = "https://www.minecraft.net/en-us/eula"

// eulaStore is the slice of the store requireMinecraftEula needs.
type eulaStore interface {
	MinecraftEulaAccepted(userID string) (bool, error)
	AcceptMinecraftEula(userID string) error
}

// requireMinecraftEula refuses an install until the caller has accepted the
// Minecraft EULA, and records the acceptance the first time they do. The node
// writes eula=true into every server it installs; that line is a declaration
// by the person who runs the server, and until now nobody had made it - the
// platform simply wrote it. Once per account: the EULA binds the person, not a
// particular server. Reports whether the request may continue.
func requireMinecraftEula(w http.ResponseWriter, st eulaStore, userID string, accept bool) bool {
	if userID == "" {
		handlers.SendJSONError(w, "Not signed in", http.StatusUnauthorized)
		return false
	}
	accepted, err := st.MinecraftEulaAccepted(userID)
	if err != nil {
		log.Printf("minecraft eula: read %s: %v", userID, err)
		handlers.SendJSONError(w, "Could not check the Minecraft EULA acceptance", http.StatusInternalServerError)
		return false
	}
	if accepted {
		return true
	}
	if !accept {
		handlers.SendJSONErrorCode(w,
			"Accept the Minecraft EULA ("+minecraftEulaURL+") to install a server. Over the API, send \"acceptMinecraftEula\": true once.",
			"minecraft_eula_required", http.StatusConflict)
		return false
	}
	if err := st.AcceptMinecraftEula(userID); err != nil {
		log.Printf("minecraft eula: record %s: %v", userID, err)
		handlers.SendJSONError(w, "Could not record the Minecraft EULA acceptance", http.StatusInternalServerError)
		return false
	}
	return true
}
