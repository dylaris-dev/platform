// Package bundle writes and reads a platform backup bundle: everything the
// operator selected, sealed under their backup passphrase.
package bundle

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Chunked AES-256-GCM, because a bundle does not fit in memory.
//
// GCM seals a whole message at once, and a platform bundle is a database dump
// plus every selected world - gigabytes. So the stream is cut into chunks, each
// sealed on its own, and the framing has to answer the three questions a single
// Seal answers for free:
//
//   - Was a chunk altered? GCM's tag answers that per chunk.
//   - Were chunks reordered or replayed? The chunk's index is authenticated as
//     additional data, so a chunk only verifies in the position it was written.
//   - Was the stream CUT SHORT? This is the one a naive chunked format gets
//     wrong. Every chunk carries a final flag in its additional data, and the
//     reader reports EOF only after reading a chunk marked final. A truncated
//     bundle is an error, not a short read - otherwise half a database restores
//     as if it were all of it.
const (
	chunkSize    = 64 * 1024
	nonceBaseLen = 8 // the random half; the other four bytes are the counter
	lenPrefix    = 4
)

// maxChunkCipher bounds what a length prefix may ask us to allocate. The prefix
// comes out of a file somebody handed us, so without this a four-byte header
// field is a request for four gigabytes.
const maxChunkCipher = chunkSize + 64

// ErrTruncated is a bundle that ends before the chunk marked final. It is
// separate from a decryption failure on purpose: a wrong passphrase and a
// half-downloaded file need different words in front of an operator.
var ErrTruncated = errors.New("bundle: the stream ends before its final chunk")

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// chunkAAD authenticates a chunk's position and whether it ends the stream.
func chunkAAD(counter uint32, final bool) []byte {
	aad := make([]byte, 5)
	binary.BigEndian.PutUint32(aad, counter)
	if final {
		aad[4] = 1
	}
	return aad
}

func chunkNonce(base []byte, counter uint32) []byte {
	nonce := make([]byte, 12)
	copy(nonce, base)
	binary.BigEndian.PutUint32(nonce[nonceBaseLen:], counter)
	return nonce
}

// streamWriter encrypts everything written to it. Close is NOT optional: it
// writes the final chunk, and without it the result is a bundle every reader
// correctly refuses.
type streamWriter struct {
	w       io.Writer
	gcm     cipher.AEAD
	base    []byte
	counter uint32
	buf     []byte
	closed  bool
	err     error
}

func newStreamWriter(w io.Writer, key, base []byte) (*streamWriter, error) {
	if len(base) != nonceBaseLen {
		return nil, fmt.Errorf("bundle: nonce base must be %d bytes", nonceBaseLen)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &streamWriter{w: w, gcm: gcm, base: base, buf: make([]byte, 0, chunkSize)}, nil
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.closed {
		return 0, errors.New("bundle: write after close")
	}
	written := 0
	for len(p) > 0 {
		n := chunkSize - len(s.buf)
		if n > len(p) {
			n = len(p)
		}
		s.buf = append(s.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(s.buf) == chunkSize {
			if err := s.flush(false); err != nil {
				s.err = err
				return written, err
			}
		}
	}
	return written, nil
}

func (s *streamWriter) flush(final bool) error {
	ct := s.gcm.Seal(nil, chunkNonce(s.base, s.counter), s.buf, chunkAAD(s.counter, final))
	var hdr [lenPrefix]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(ct); err != nil {
		return err
	}
	s.buf = s.buf[:0]
	s.counter++
	return nil
}

// Close writes whatever is buffered as the final chunk. The final chunk may be
// empty - a stream whose length is an exact multiple of the chunk size still
// needs its end marked.
func (s *streamWriter) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

// streamReader decrypts a stream written by streamWriter.
type streamReader struct {
	r       io.Reader
	gcm     cipher.AEAD
	base    []byte
	counter uint32
	buf     []byte
	done    bool
	err     error
}

func newStreamReader(r io.Reader, key, base []byte) (*streamReader, error) {
	if len(base) != nonceBaseLen {
		return nil, fmt.Errorf("bundle: nonce base must be %d bytes", nonceBaseLen)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &streamReader{r: r, gcm: gcm, base: base}, nil
}

func (s *streamReader) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		if s.done {
			return 0, io.EOF
		}
		if err := s.next(); err != nil {
			s.err = err
			return 0, err
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *streamReader) next() error {
	var hdr [lenPrefix]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// The stream ran out without a chunk marked final.
			return ErrTruncated
		}
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxChunkCipher {
		return fmt.Errorf("bundle: chunk length %d is not plausible", n)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(s.r, ct); err != nil {
		return ErrTruncated
	}

	nonce := chunkNonce(s.base, s.counter)
	// Try the chunk as a middle chunk first, then as the final one. Which it is
	// is authenticated, not declared: a stream cannot be cut short by rewriting
	// a flag, because the flag is inside the tag.
	pt, err := s.gcm.Open(nil, nonce, ct, chunkAAD(s.counter, false))
	if err != nil {
		pt, err = s.gcm.Open(nil, nonce, ct, chunkAAD(s.counter, true))
		if err != nil {
			return fmt.Errorf("bundle: chunk %d does not verify", s.counter)
		}
		s.done = true
	}
	s.counter++
	s.buf = pt
	return nil
}
