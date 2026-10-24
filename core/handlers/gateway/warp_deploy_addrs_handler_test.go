package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/services"
)

// Every node kit shown without a fresh enroll token reads Core's gRPC pin from
// here. It follows GRPC_TLS_ENABLED exactly as the enroll-token answer does: the
// pin while TLS is on, "" while the channel is plaintext - which the panel writes
// as GRPC_TLS_ENABLED "false", so a pin shown while TLS is off would be as wrong
// as none while it is on. The field is always present: the panel reads a missing
// one as "not known", never as plaintext.
func TestGetDeployConfigCarriesTheGRPCPinOnlyWhileTLSIsOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{"TLS on", true, "ab12cd34"},
		{"TLS off", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewWarpHandler(&handlers.AppState{GRPCTLSEnabled: tc.enabled, GRPCTLSFingerprint: "ab12cd34"}, nil)
			rec := httptest.NewRecorder()
			h.GetDeployConfig(rec, httptest.NewRequest(http.MethodGet, "/api/warp/deploy-config", nil))

			var body struct {
				Config map[string]any `json:"config"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			got, ok := body.Config["grpcTlsFingerprint"]
			if !ok {
				t.Fatalf("the config carries no grpcTlsFingerprint: %s", rec.Body.String())
			}
			if got != tc.want {
				t.Errorf("grpcTlsFingerprint = %v, want %q", got, tc.want)
			}
		})
	}
}

// The enroll and assignment handlers both go through this, so the panel snippet
// and the warp client can never be handed different answers.
func TestStampOverlayAddrs(t *testing.T) {
	t.Setenv("REDIS_ADDR", "10.20.0.5:6379")
	t.Setenv("CORE_SERVICE_NAME", "10.20.0.4")
	t.Setenv("DYLARIS_GRPC_PORT", "25501")

	res := services.EnrollResult{WGIP: "10.0.99.7"}
	stampOverlayAddrs(&res)

	if res.CoreGRPCAddr != "10.20.0.4:25501" {
		t.Errorf("CoreGRPCAddr = %q", res.CoreGRPCAddr)
	}
	if res.RedisAddr != "10.20.0.5:6379" {
		t.Errorf("RedisAddr = %q", res.RedisAddr)
	}
	if res.WGIP != "10.0.99.7" {
		t.Errorf("stamping must not disturb the rest of the response, WGIP = %q", res.WGIP)
	}
}
