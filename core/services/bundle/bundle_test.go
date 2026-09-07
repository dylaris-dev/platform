package bundle

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
)

const pass = "a documented backup passphrase"

func writeBundle(t *testing.T, m *Manifest, members map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, pass, "2026.09.07.13")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteManifest(m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	for name, body := range members {
		if err := w.AddBytes(name, []byte(body)); err != nil {
			t.Fatalf("AddBytes %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

func sampleManifest() *Manifest {
	return &Manifest{
		Schema:        Schema,
		Source:        "2026.09.07.13",
		ClusterSecret: "the-source-instances-cluster-secret",
		Selection: models.PlatformBackupSelection{
			Database: true,
			Servers:  models.PlatformBackupServers{Mode: models.PlatformBackupServersNone},
		},
		Components: []models.PlatformBackupComponent{
			{Kind: "database", Status: models.PlatformBackupIncluded, SizeBytes: 11},
		},
	}
}

func TestBundleRoundTrip(t *testing.T) {
	raw := writeBundle(t, sampleManifest(), map[string]string{"database.sql": "CREATE ..."})

	h, m, tr, err := Open(bytes.NewReader(raw), pass)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if h.Schema != Schema || h.Source != "2026.09.07.13" {
		t.Errorf("header = %+v", h)
	}
	if m.ClusterSecret != "the-source-instances-cluster-secret" {
		t.Error("the source cluster secret did not survive; a restore cannot reseal without it")
	}
	if len(m.Components) != 1 || m.Components[0].Kind != "database" {
		t.Errorf("components = %+v", m.Components)
	}

	entry, err := tr.Next()
	if err != nil {
		t.Fatalf("first entry after the manifest: %v", err)
	}
	if entry.Name != "database.sql" {
		t.Fatalf("entry = %q", entry.Name)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != "CREATE ..." {
		t.Fatalf("body = %q", body)
	}
}

// The property the whole design turns on: a restore refuses a wrong passphrase
// before it writes anything, and it does so from the header rather than by
// failing somewhere inside a half-applied database.
func TestBundleRefusesTheWrongPassphrase(t *testing.T) {
	raw := writeBundle(t, sampleManifest(), map[string]string{"database.sql": "CREATE ..."})

	h, m, tr, err := Open(bytes.NewReader(raw), "not the passphrase at all")
	if !errors.Is(err, crypto.ErrWrongPassphrase) {
		t.Fatalf("err = %v, want ErrWrongPassphrase", err)
	}
	if m != nil || tr != nil {
		t.Fatal("a wrong passphrase still returned payload")
	}
	// The header is still handed back: an operator who mistyped needs to see
	// which bundle they are looking at, and none of it is secret.
	if h == nil {
		t.Fatal("the plaintext header was withheld from a failed open")
	}
}

// A bundle has to be identifiable without its passphrase, or an operator
// holding several of them cannot tell which is which.
func TestReadHeaderNeedsNoPassphrase(t *testing.T) {
	raw := writeBundle(t, sampleManifest(), nil)

	h, _, err := ReadHeader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Source != "2026.09.07.13" || h.CreatedAt.IsZero() {
		t.Errorf("header = %+v", h)
	}
	if h.KDF == nil || h.KDF.Salt == "" || h.KDF.Verifier == "" {
		t.Error("the header carries no key derivation; no other instance could open this bundle")
	}
}

// Nothing that describes the platform may sit outside the encryption. A
// component list naming servers and owners is a description of the customer
// base, and a bundle gets copied to wherever backups go.
func TestPlaintextHeaderLeaksNothingAboutTheContents(t *testing.T) {
	m := sampleManifest()
	m.Components = []models.PlatformBackupComponent{
		{Kind: "server", Ref: "alices-secret-project", Status: models.PlatformBackupIncluded},
	}
	raw := writeBundle(t, m, map[string]string{"database.sql": "SELECT 'bob@example.test'"})

	// Only the two plaintext lines.
	head := raw
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		if j := bytes.IndexByte(raw[i+1:], '\n'); j >= 0 {
			head = raw[:i+1+j+1]
		}
	}
	for _, secret := range []string{
		"alices-secret-project",
		"bob@example.test",
		"the-source-instances-cluster-secret",
		pass,
	} {
		if bytes.Contains(head, []byte(secret)) {
			t.Errorf("the plaintext header contains %q", secret)
		}
	}
	// And not anywhere else in the file either, which is what the encryption is
	// actually for.
	if bytes.Contains(raw, []byte("alices-secret-project")) {
		t.Error("a server name appears in the clear in the bundle")
	}
	if bytes.Contains(raw, []byte("the-source-instances-cluster-secret")) {
		t.Error("the source cluster secret appears in the clear in the bundle")
	}
}

func TestOpenRefusesThingsThatAreNotBundles(t *testing.T) {
	cases := map[string]string{
		"empty":                  "",
		"random text":            "hello\nworld\n",
		"right magic, no header": Magic + " 1\n",
		"header not json":        Magic + " 1\nnot json\n",
		"no trailing newline":    Magic + " 1",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := Open(strings.NewReader(body), pass); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A bundle from a newer Dylaris must say so rather than be parsed hopefully:
// the payload framing itself may have changed, so guessing means decrypting
// something under the wrong rules.
func TestOpenRefusesAnUnknownSchema(t *testing.T) {
	raw := writeBundle(t, sampleManifest(), nil)
	future := bytes.Replace(raw, []byte(`"schema":1`), []byte(`"schema":99`), 1)
	if bytes.Equal(future, raw) {
		t.Fatal("the schema field was not where the test expected it")
	}
	if _, _, _, err := Open(bytes.NewReader(future), pass); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("err = %v, want ErrUnknownSchema", err)
	}
}

// A bundle whose upload was cut short must fail, not restore a shorter
// database. The stream tests cover the framing; this one covers that a Writer
// left unclosed produces exactly that failure end to end.
func TestOpenRefusesATruncatedBundle(t *testing.T) {
	raw := writeBundle(t, sampleManifest(), map[string]string{
		"database.sql": strings.Repeat("INSERT INTO t VALUES (1);\n", 20000),
	})
	cut := raw[:len(raw)-4096]

	_, _, tr, err := Open(bytes.NewReader(cut), pass)
	if err != nil {
		return // refused at open, which is fine
	}
	// Otherwise it must fail while reading, and never report a clean EOF.
	for {
		if _, err := tr.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("a truncated bundle read to a clean end")
			}
			return
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return
		}
	}
}

func TestAddReaderRefusesAShortBody(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriter(&out, pass, "test")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteManifest(sampleManifest()); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	// A producer that dies halfway must not become a member whose header says
	// one size and whose body is another - tar pads the difference, and the
	// bundle then restores less than it claims with nothing reporting it.
	if err := w.AddReader("database.sql", 1000, strings.NewReader("only this much")); err == nil {
		t.Fatal("a short body was accepted")
	}
}

func TestNewWriterRefusesAShortPassphrase(t *testing.T) {
	var out bytes.Buffer
	if _, err := NewWriter(&out, "short", "test"); !errors.Is(err, crypto.ErrPassphraseTooShort) {
		t.Fatalf("err = %v, want ErrPassphraseTooShort", err)
	}
	if out.Len() != 0 {
		t.Error("a refused bundle still wrote its header")
	}
}

// Two bundles written with the SAME passphrase must not share a key, or
// recovering one recovers all of them.
func TestTwoBundlesDoNotShareAKey(t *testing.T) {
	a := writeBundle(t, sampleManifest(), nil)
	b := writeBundle(t, sampleManifest(), nil)

	ha, _, _ := ReadHeader(bytes.NewReader(a))
	hb, _, _ := ReadHeader(bytes.NewReader(b))
	if ha.KDF.Salt == hb.KDF.Salt {
		t.Error("two bundles share a salt")
	}
	if ha.NonceBase == hb.NonceBase {
		t.Error("two bundles share a nonce base")
	}
}
