package services

// GatewayRoutingEnabled reports whether platform routing currently uses the
// gateway. routing_mode "gateway" and "both" both run the gateway; "ip_port"
// (the default, also the value used on empty/error) does not.
//
// It is the handlers' one answer to "is the gateway routing traffic" (route
// gates, node address redaction, placement, health). Moved here from the
// AppState method when the route gates left the handlers package.
func GatewayRoutingEnabled(st interface {
	GetSetting(key string) (string, error)
}) bool {
	mode, _ := st.GetSetting("routing_mode")
	if mode == "" {
		mode = "ip_port"
	}
	return mode == "gateway" || mode == "both"
}
