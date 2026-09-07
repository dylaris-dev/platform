package bundle

import (
	"archive/tar"
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
)

// The bundle file.
//
//	line 1   DYLARIS-BUNDLE <schema>
//	line 2   the header, one line of JSON
//	rest     the chunked, encrypted payload: a tar
//
// Two plaintext lines and nothing else. They exist so that a bundle can be
// IDENTIFIED without its passphrase - which version wrote it, when, and what
// key derivation it needs - and so a restore can refuse a wrong passphrase
// before it touches anything. Everything that says something about the platform
// is inside the encrypted part, including the list of what the bundle contains:
// a component list naming servers and owners is a description of the customer
// base, and it does not belong on the outside of a file that gets copied to
// wherever backups go.
const (
	// Magic is the first token of the file, so `head -1` identifies it.
	Magic = "DYLARIS-BUNDLE"
	// Schema is the FORMAT version. A reader that does not know a schema must
	// say so rather than guess: the payload framing may have changed.
	Schema = 1
	// ManifestEntry is the first tar entry inside the encrypted payload.
	ManifestEntry = "manifest.json"
)

// maxHeaderLine bounds the two plaintext lines. They are a few hundred bytes;
// anything larger is not a header, and reading it on the strength of an
// unterminated line is how a file becomes a memory limit.
const maxHeaderLine = 64 * 1024

// maxManifestBytes bounds the manifest read out of an untrusted payload.
const maxManifestBytes = 8 << 20

// ErrUnknownSchema is a bundle from a newer Dylaris.
var ErrUnknownSchema = errors.New("bundle: written by a newer Dylaris than this one")

// ErrNotABundle is a file that is not one.
var ErrNotABundle = errors.New("bundle: not a Dylaris backup bundle")

// Header is the plaintext description.
type Header struct {
	Schema    int       `json:"schema"`
	CreatedAt time.Time `json:"createdAt"`
	// Source is the release the writing Dylaris ran, so an operator reading a
	// bundle they cannot open still knows where it came from.
	Source string `json:"source,omitempty"`
	// KDF is how to turn the passphrase into the payload key, and how to tell a
	// wrong passphrase from a right one before writing anything.
	KDF *crypto.BundleKDF `json:"kdf"`
	// NonceBase is the random half of every chunk nonce, per bundle. Public: a
	// nonce is not a secret, it only has to be unique under one key.
	NonceBase string `json:"nonceBase"`
}

// Manifest is the first entry INSIDE the encrypted payload.
type Manifest struct {
	Schema    int       `json:"schema"`
	CreatedAt time.Time `json:"createdAt"`
	Source    string    `json:"source,omitempty"`
	// ClusterSecret is the secret the SOURCE instance was running.
	//
	// It travels because the database in this bundle holds values encrypted
	// under it, and because the warp leader's WireGuard identity is DERIVED
	// from it and stored nowhere at all - restore onto an instance with a
	// different secret and every leader comes back under a new public key while
	// every enrolled peer still points at the old one.
	//
	// The marginal risk is nil: this payload already carries every node secret
	// and every storage credential under the same passphrase. Leaving the
	// cluster secret out would not make the bundle safer, only less restorable.
	ClusterSecret string                           `json:"clusterSecret,omitempty"`
	Selection     models.PlatformBackupSelection   `json:"selection"`
	Components    []models.PlatformBackupComponent `json:"components"`
}

// Writer produces a bundle.
type Writer struct {
	out    io.Writer
	stream *streamWriter
	tw     *tar.Writer
	closed bool
}

// NewWriter writes the two plaintext lines and opens the encrypted payload.
//
// The manifest is written by the CALLER as the first entry, through
// WriteManifest, because what a run contains is only known once it has decided
// what it could actually reach.
func NewWriter(out io.Writer, passphrase, source string) (*Writer, error) {
	kdf, key, err := crypto.NewBundleKDF(passphrase)
	if err != nil {
		return nil, err
	}
	base := make([]byte, nonceBaseLen)
	if _, err := io.ReadFull(rand.Reader, base); err != nil {
		return nil, fmt.Errorf("bundle: nonce base: %w", err)
	}

	hdr := Header{
		Schema:    Schema,
		CreatedAt: time.Now().UTC(),
		Source:    source,
		KDF:       kdf,
		NonceBase: hex.EncodeToString(base),
	}
	line, err := json.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(out, "%s %d\n", Magic, Schema); err != nil {
		return nil, err
	}
	if _, err := out.Write(append(line, '\n')); err != nil {
		return nil, err
	}

	sw, err := newStreamWriter(out, key, base)
	if err != nil {
		return nil, err
	}
	return &Writer{out: out, stream: sw, tw: tar.NewWriter(sw)}, nil
}

// WriteManifest writes the manifest as the current entry. Call it first.
func (w *Writer) WriteManifest(m *Manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return w.AddBytes(ManifestEntry, b)
}

// AddBytes adds a small member held in memory.
func (w *Writer) AddBytes(name string, data []byte) error {
	if err := w.tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now().UTC(),
	}); err != nil {
		return err
	}
	_, err := w.tw.Write(data)
	return err
}

// AddReader adds a member of a KNOWN size.
//
// The size is required because tar writes it in the header, ahead of the data.
// A producer whose length is not known in advance - pg_dump, a directory walk -
// has to be spooled to a file first, and the caller does that rather than this
// package, because only the caller knows where it may put a file that large.
func (w *Writer) AddReader(name string, size int64, r io.Reader) error {
	if err := w.tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: size, ModTime: time.Now().UTC(),
	}); err != nil {
		return err
	}
	n, err := io.Copy(w.tw, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n != size {
		// tar would pad the difference and produce an archive whose header
		// disagrees with its content, which reads back as a silently shorter
		// file. Better a failed backup than a bundle that restores less than it
		// says.
		return fmt.Errorf("bundle: %s: wrote %d bytes, header says %d", name, n, size)
	}
	return nil
}

// Close finishes the tar and the encrypted stream. It is not optional: without
// the stream's final chunk, every reader correctly refuses the result.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.tw.Close(); err != nil {
		return err
	}
	return w.stream.Close()
}

// ReadHeader reads the two plaintext lines WITHOUT a passphrase, and returns
// the reader positioned at the encrypted payload.
//
// Separate from Open so a panel can say what a bundle is - when it was made,
// which release wrote it - before asking anyone to type a passphrase for it.
func ReadHeader(r io.Reader) (*Header, *bufio.Reader, error) {
	br := bufio.NewReader(r)

	magic, err := readLine(br)
	if err != nil {
		return nil, nil, ErrNotABundle
	}
	fields := strings.Fields(magic)
	if len(fields) < 2 || fields[0] != Magic {
		return nil, nil, ErrNotABundle
	}

	line, err := readLine(br)
	if err != nil {
		return nil, nil, ErrNotABundle
	}
	var h Header
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return nil, nil, ErrNotABundle
	}
	if h.Schema != Schema {
		return nil, nil, fmt.Errorf("%w (schema %d)", ErrUnknownSchema, h.Schema)
	}
	return &h, br, nil
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > maxHeaderLine {
		return "", errors.New("bundle: header line is too long")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Open reads the header, derives the key and returns the manifest plus a tar
// reader positioned at the entry after it.
//
// A wrong passphrase is reported HERE, from the header's verifier, before a
// single byte of payload is decrypted and long before anything is written.
func Open(r io.Reader, passphrase string) (*Header, *Manifest, *tar.Reader, error) {
	h, br, err := ReadHeader(r)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := h.KDF.DeriveKey(passphrase)
	if err != nil {
		return h, nil, nil, err
	}
	base, err := hex.DecodeString(h.NonceBase)
	if err != nil {
		return h, nil, nil, errors.New("bundle: nonce base is unreadable")
	}
	sr, err := newStreamReader(br, key, base)
	if err != nil {
		return h, nil, nil, err
	}

	tr := tar.NewReader(sr)
	hdr, err := tr.Next()
	if err != nil {
		return h, nil, nil, fmt.Errorf("bundle: payload: %w", err)
	}
	if hdr.Name != ManifestEntry {
		return h, nil, nil, fmt.Errorf("bundle: first entry is %q, want %s", hdr.Name, ManifestEntry)
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes))
	if err != nil {
		return h, nil, nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return h, nil, nil, fmt.Errorf("bundle: manifest: %w", err)
	}
	return h, &m, tr, nil
}
