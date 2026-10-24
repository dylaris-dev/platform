package beam

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// beamManifestServer serves latest.json and latest.json.sig, so a test can hand
// out a correctly signed manifest, a wrongly signed one, or none at all.
func beamManifestServer(t *testing.T, body, sig []byte, serveSig bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	if serveSig {
		mux.HandleFunc("/latest.json.sig", func(w http.ResponseWriter, r *http.Request) {
			w.Write(sig)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const beamTestManifest = `{"version":"1.2.3","minVersion":"1.0.0","platforms":{"windows-amd64":{"url":"https://example.invalid/beam.exe"}}}`

// TestFetchVerifiedBeamPlatformURL_RequiresAValidSignature is the security half
// of the download fix.
//
// The download endpoint used to fetch latest.json and unmarshal it WITHOUT ever
// requesting latest.json.sig, so the URL it then streamed an executable from
// came out of a document nothing had authenticated - even though Core already
// carried verifyBeamManifest and a comment saying the manifest is read only
// after the signature verifies.
func TestFetchVerifiedBeamPlatformURL_RequiresAValidSignature(t *testing.T) {
	// httptest binds 127.0.0.1, which the beam fetch guard refuses on purpose.
	allowLoopbackBeamDial(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	body := []byte(beamTestManifest)
	goodSig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body)))

	t.Run("valid signature yields the url", func(t *testing.T) {
		srv := beamManifestServer(t, body, goodSig, true)
		got, err := fetchVerifiedBeamPlatformURL(context.Background(), srv.URL+"/latest.json", pubB64, "windows-amd64")
		if err != nil {
			t.Fatalf("fetchVerifiedBeamPlatformURL: %v", err)
		}
		if want := "https://example.invalid/beam.exe"; got != want {
			t.Errorf("url = %q, want %q", got, want)
		}
	})

	t.Run("signature from another key is refused", func(t *testing.T) {
		_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
		badSig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(otherPriv, body)))
		srv := beamManifestServer(t, body, badSig, true)
		if _, err := fetchVerifiedBeamPlatformURL(context.Background(), srv.URL+"/latest.json", pubB64, "windows-amd64"); err == nil {
			t.Fatal("a manifest signed by a foreign key was accepted; Core would stream an attacker-chosen binary")
		}
	})

	t.Run("tampered body is refused", func(t *testing.T) {
		tampered := []byte(`{"version":"1.2.3","minVersion":"1.0.0","platforms":{"windows-amd64":{"url":"https://evil.invalid/beam.exe"}}}`)
		srv := beamManifestServer(t, tampered, goodSig, true)
		if _, err := fetchVerifiedBeamPlatformURL(context.Background(), srv.URL+"/latest.json", pubB64, "windows-amd64"); err == nil {
			t.Fatal("a manifest whose URL was swapped after signing was accepted")
		}
	})

	t.Run("missing signature is refused, not ignored", func(t *testing.T) {
		srv := beamManifestServer(t, body, nil, false)
		if _, err := fetchVerifiedBeamPlatformURL(context.Background(), srv.URL+"/latest.json", pubB64, "windows-amd64"); err == nil {
			t.Fatal("an unsigned manifest was accepted; this is the exact pre-fix behaviour")
		}
	})

	t.Run("unknown platform is refused", func(t *testing.T) {
		srv := beamManifestServer(t, body, goodSig, true)
		if _, err := fetchVerifiedBeamPlatformURL(context.Background(), srv.URL+"/latest.json", pubB64, "linux-arm64"); err == nil {
			t.Fatal("a platform absent from the signed manifest returned a url")
		}
	})

	// The download route is unauthenticated and browser-facing, so "we do not
	// build for your platform" and "the release feed is down" must not read the
	// same. Only the first is the visitor's to act on; while GitHub is
	// unreachable EVERY platform looks unbuilt, and telling a Windows user there
	// is no Windows build would be false AND would hide the outage.
	t.Run("no build for this platform is distinguishable from a broken feed", func(t *testing.T) {
		srv := beamManifestServer(t, body, goodSig, true)
		_, _, err := fetchVerifiedBeamPlatformArtifact(context.Background(), srv.URL+"/latest.json", pubB64, "darwin-arm64")
		if !errors.Is(err, errBeamPlatformNotInManifest) {
			t.Errorf("absent platform: err = %v, want errBeamPlatformNotInManifest", err)
		}

		// A platform that IS built, but whose manifest cannot be verified, must
		// NOT come back as "no build for your platform".
		unsigned := beamManifestServer(t, body, nil, false)
		_, _, err = fetchVerifiedBeamPlatformArtifact(context.Background(), unsigned.URL+"/latest.json", pubB64, "windows-amd64")
		if err == nil {
			t.Fatal("an unsigned manifest was accepted")
		}
		if errors.Is(err, errBeamPlatformNotInManifest) {
			t.Error("an unverifiable manifest was reported as \"no build for this platform\"; " +
				"every visitor would be told a permanent falsehood and the outage would be invisible")
		}
	})
}

// TestBeamBinaryClientVerifiesTLS pins the other half: the client that fetches
// the executable must not skip certificate verification. It used to, justified
// by a self-signed relay on the overlay that has not been in this path since
// eeff445 - leaving Core fetching an executable from the public internet with
// verification disabled.
func TestBeamBinaryClientVerifiesTLS(t *testing.T) {
	tr, ok := beamBinaryClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", beamBinaryClient.Transport)
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("beamBinaryClient still skips TLS verification while fetching an executable")
	}

	// And prove it end to end against a server with an untrusted cert.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("malicious binary"))
	}))
	t.Cleanup(srv.Close)
	resp, err := beamBinaryClient.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("fetched from a server with an untrusted certificate without error")
	}
}
