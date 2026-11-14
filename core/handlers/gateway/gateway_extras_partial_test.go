package gateway

import (
	"encoding/base64"
	"net/http"
	"testing"
)

// Review M-2: Bedrock is on (provider yes, key stored) but the key cannot be
// handed to Redis. The answer is success with the restart owed and a warning,
// not an error inviting a retry; a retry that finds the key still missing in
// Redis publishes it and still owes the restart; once it is there, a re-save
// owes nothing.
func TestRemoteBedrockKeyPublishFailureAnswersOnWithAWarning(t *testing.T) {
	e := newRemoteExtras(t)
	e.mr.SetError("ERR redis is down")
	rec, out := bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false)
	e.mr.SetError("")
	if rec.Code != http.StatusOK || out["restartRequired"] != true || out["warning"] != remoteBedrockKeyLate {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	want := providerKey(t, e.fake, remoteAddr)
	if e.rs.row(remoteAddr).Edition != "both" || string(e.rs.key) != string(want) || e.publishedKey() != "" {
		t.Fatalf("edition %q, stored %s, published %v", e.rs.row(remoteAddr).Edition, keyID(e.rs.key), e.publishedKey() != "")
	}

	rec, out = bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false)
	if _, warned := out["warning"]; rec.Code != http.StatusOK || out["restartRequired"] != true || warned {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
	if e.publishedKey() != base64.StdEncoding.EncodeToString(want) {
		t.Fatalf("retry did not publish the stored key (want %s)", keyID(want))
	}

	rec, out = bedrockCall(e.voiceSwitchEnv, http.MethodPut, bedrockBody(remoteAddr, true, true), false)
	if _, owed := out["restartRequired"]; rec.Code != http.StatusOK || owed {
		t.Fatalf("re-save with the key in place: %d %s", rec.Code, rec.Body.String())
	}
}

// Review M-8: with the gateway mode unread, turning voice off cannot know
// which side holds the voice; it answers 503 instead of a success that left
// the provider's voice on. Local mode stays as it was
// (TestVoiceLocalModeUnchanged).
func TestRemoteVoiceOffUnknownModeAnswers503(t *testing.T) {
	e := newRemoteExtras(t)
	if rec, _ := e.put(t, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("on: %d %s", rec.Code, rec.Body.String())
	}
	before := len(e.fake.Calls())
	e.h.d.GatewayRemote = func() (bool, bool) { return false, false }
	rec, out := e.put(t, `{"enabled":false}`)
	if rec.Code != http.StatusServiceUnavailable || out["code"] != "gateway_provider_unknown" {
		t.Fatalf("off: %d %s", rec.Code, rec.Body.String())
	}
	if r := e.rs.row(remoteAddr); r.VoicePort == nil || len(e.fake.Calls()) != before {
		t.Fatalf("an unread mode changed something: row %+v, calls %v", r, e.fake.Calls()[before:])
	}
}
