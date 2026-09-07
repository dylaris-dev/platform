package bundle

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func testKey() []byte  { return bytes.Repeat([]byte{0x2b}, 32) }
func testBase() []byte { return []byte{1, 2, 3, 4, 5, 6, 7, 8} }

func seal(t *testing.T, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := newStreamWriter(&out, testKey(), testBase())
	if err != nil {
		t.Fatalf("newStreamWriter: %v", err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

func open(t *testing.T, sealed, key []byte) ([]byte, error) {
	t.Helper()
	r, err := newStreamReader(bytes.NewReader(sealed), key, testBase())
	if err != nil {
		t.Fatalf("newStreamReader: %v", err)
	}
	return io.ReadAll(r)
}

func TestStreamRoundTrip(t *testing.T) {
	sizes := []int{
		0, 1, 1000,
		chunkSize - 1,
		// Exactly one chunk, which is the case a format without an explicit
		// end marker gets wrong: there is nothing left over to signal the end.
		chunkSize,
		chunkSize + 1,
		3 * chunkSize,
		3*chunkSize + 17,
	}
	for _, n := range sizes {
		plain := make([]byte, n)
		for i := range plain {
			plain[i] = byte(i * 7)
		}
		sealed := seal(t, plain)
		got, err := open(t, sealed, testKey())
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip differs", n)
		}
	}
}

func TestStreamWriteInManyPieces(t *testing.T) {
	// A tar writer produces small, uneven writes; the chunking must not depend
	// on how the caller happens to slice its input.
	var out bytes.Buffer
	w, err := newStreamWriter(&out, testKey(), testBase())
	if err != nil {
		t.Fatalf("newStreamWriter: %v", err)
	}
	var want []byte
	for i := 0; i < 5000; i++ {
		piece := bytes.Repeat([]byte{byte(i)}, i%97)
		want = append(want, piece...)
		if _, err := w.Write(piece); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := open(t, out.Bytes(), testKey())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
}

// The failure this format exists to catch. A bundle cut short - a download that
// stopped, an upload that ran out of quota - must be an error, not a shorter
// database.
func TestStreamRefusesATruncatedBundle(t *testing.T) {
	plain := bytes.Repeat([]byte("world data "), 20000) // several chunks
	sealed := seal(t, plain)

	cases := map[string][]byte{
		"cut mid-chunk":           sealed[:len(sealed)-100],
		"cut at a chunk boundary": sealed[:lenPrefix+chunkSize+16],
		"cut to nothing":          sealed[:0],
		"only a length prefix":    sealed[:lenPrefix],
	}
	for name, cut := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := open(t, cut, testKey())
			if err == nil {
				t.Fatalf("a truncated bundle read back %d bytes with no error", len(got))
			}
			if !errors.Is(err, ErrTruncated) && name != "cut mid-chunk" {
				t.Logf("err = %v", err)
			}
		})
	}
}

// A writer that never closes has produced the same thing as a truncated file,
// and must be refused for the same reason.
func TestStreamRefusesAStreamThatWasNeverClosed(t *testing.T) {
	var out bytes.Buffer
	w, err := newStreamWriter(&out, testKey(), testBase())
	if err != nil {
		t.Fatalf("newStreamWriter: %v", err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{9}, 3*chunkSize)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// No Close.
	if _, err := open(t, out.Bytes(), testKey()); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestStreamRefusesATamperedChunk(t *testing.T) {
	plain := bytes.Repeat([]byte("x"), 3*chunkSize)
	sealed := seal(t, plain)

	// One bit, in the middle of the first chunk's ciphertext.
	tampered := append([]byte{}, sealed...)
	tampered[lenPrefix+100] ^= 0x01
	if _, err := open(t, tampered, testKey()); err == nil {
		t.Fatal("a tampered chunk verified")
	}
}

// Each chunk authenticates its own position, so a bundle cannot be rearranged
// into a different database.
func TestStreamRefusesReorderedChunks(t *testing.T) {
	plain := make([]byte, 2*chunkSize)
	for i := range plain {
		plain[i] = byte(i)
	}
	sealed := seal(t, plain)

	first := sealed[:lenPrefix+chunkSize+16]
	second := sealed[lenPrefix+chunkSize+16 : 2*(lenPrefix+chunkSize+16)]
	rest := sealed[2*(lenPrefix+chunkSize+16):]

	swapped := append([]byte{}, second...)
	swapped = append(swapped, first...)
	swapped = append(swapped, rest...)

	if _, err := open(t, swapped, testKey()); err == nil {
		t.Fatal("reordered chunks verified")
	}
}

// Dropping the final chunk and letting the one before it stand as the end is
// the attack the final flag exists for: without it, the reader would see a
// well-formed stream that is simply shorter.
func TestStreamRefusesADroppedFinalChunk(t *testing.T) {
	plain := make([]byte, 2*chunkSize)
	sealed := seal(t, plain)
	// Everything but the last chunk. Each full chunk is prefix + plaintext + tag.
	full := lenPrefix + chunkSize + 16
	if _, err := open(t, sealed[:2*full], testKey()); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestStreamRefusesTheWrongKey(t *testing.T) {
	sealed := seal(t, []byte("the platform database"))
	wrong := bytes.Repeat([]byte{0x2c}, 32)
	if _, err := open(t, sealed, wrong); err == nil {
		t.Fatal("the wrong key opened the stream")
	}
}

// A length prefix is four bytes out of a file somebody handed us. Without a
// ceiling it is a request to allocate whatever it says.
func TestStreamRefusesAnImplausibleChunkLength(t *testing.T) {
	hostile := []byte{0xff, 0xff, 0xff, 0xff}
	if _, err := open(t, hostile, testKey()); err == nil {
		t.Fatal("a four-gigabyte chunk length was accepted")
	}
}

func TestStreamWriterRejectsAWrongSizedNonceBase(t *testing.T) {
	var out bytes.Buffer
	if _, err := newStreamWriter(&out, testKey(), []byte{1, 2, 3}); err == nil {
		t.Fatal("a short nonce base was accepted")
	}
	if _, err := newStreamReader(&out, testKey(), []byte{1, 2, 3}); err == nil {
		t.Fatal("a short nonce base was accepted on read")
	}
}
