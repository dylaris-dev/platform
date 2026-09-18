package handlers

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"
)

// Size caps bound the memory a single server-pack render can consume, since it
// is reachable from the public share endpoint. A deflate zip-bomb inside an
// owner-uploaded stored zip could otherwise decompress fully into RAM. These
// mirror the 512 MiB per-entry cap the sibling unzip helpers already apply.
const (
	maxServerPackEntryBytes = 512 << 20 // 512 MiB per inner file
	maxServerPackTotalBytes = 2 << 30   // 2 GiB assembled pack
)

// renderServerPack builds a plain .zip of a build's SERVER-SIDE content + configs
// (every entry whose side is not client-only), each file placed at its
// .minecraft-relative path so a server operator can extract it into the server
// root. Modrinth-linked content is downloaded (hash-verified) and embedded;
// uploaded content is read from storage and its inner files copied in. Unlike the
// .mrpack render, a plain zip has no reference mechanism, so every file is
// embedded. That is heavier and only produced on a share-link download.
//
// It deliberately does NOT include a loader/server jar: the spec scopes a
// server-pack to "server-side content + configs". The operator installs the
// loader (Fabric/Forge/...) via the normal server-install flow.
// renderServerPack assembles the server pack zip and writes it to dst as it
// goes, so peak memory is one content entry rather than the whole pack. It used
// to build the entire archive in a bytes.Buffer and return it - up to the 2 GiB
// cap held in RAM, per request, on the unauthenticated share route. Streaming
// bounds that to the largest single entry (itself capped) regardless of pack
// size or concurrency.
//
// The cost of streaming: once bytes have flowed to dst the HTTP status is
// committed, so an error partway through cannot become a clean 500. The caller
// tracks whether anything was written and reports a clean error only while none
// has. Every security check (unsafe zip entries, the size caps, unsafe target
// paths) still runs BEFORE its entry is written, so a rejected entry aborts the
// stream without its bytes ever reaching the client.
func (h *PacksHandler) renderServerPack(ctx context.Context, content []models.BuildContentEntry, dst io.Writer) error {
	prov, err := h.state.buildModpackStorageProvider()
	if err != nil {
		return err
	}
	if prov == nil {
		return fmt.Errorf("modpack storage not configured")
	}
	zw := zip.NewWriter(dst)
	total := &packSizeBudget{limit: maxServerPackTotalBytes}
	for _, e := range content {
		if e.Side == models.SideClient {
			continue // client-only content is not part of a server pack
		}
		switch e.Source {
		case models.SourceUpload:
			if err := h.streamUploadContent(ctx, zw, prov, e, total); err != nil {
				return err
			}
		case models.SourceModrinth:
			if err := streamModrinthContent(ctx, zw, e, total); err != nil {
				return err
			}
		default:
			return fmt.Errorf("content %q has unsupported source %q", e.ModSlug, e.Source)
		}
	}
	return zw.Close()
}

// streamUploadContent copies a stored content zip to a temp file and re-serves
// its inner files into the output zip WITHOUT ever holding the whole object in
// memory. A zip cannot be read as a forward stream - its index is at the end -
// so the object is spooled to disk (bounded by io.Copy's buffer) and read from
// there, which is what keeps memory flat no matter how large the content is.
func (h *PacksHandler) streamUploadContent(ctx context.Context, zw *zip.Writer, prov modpack.ModpackStorageProvider, e models.BuildContentEntry, total *packSizeBudget) error {
	if e.StorageKey == "" {
		return fmt.Errorf("upload content %q missing storage key", e.ModSlug)
	}
	rc, _, err := prov.Stream(ctx, e.StorageKey)
	if err != nil {
		return fmt.Errorf("read stored content %q: %w", e.ModSlug, err)
	}
	// The whole stored object may not exceed the pack total; that is the only
	// bound on a single content zip, and it caps the temp file too.
	tmp, size, err := spoolToTemp(rc, maxServerPackTotalBytes)
	rc.Close()
	if err != nil {
		return fmt.Errorf("stage stored content %q: %w", e.ModSlug, err)
	}
	defer cleanupTemp(tmp)

	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return fmt.Errorf("unzip stored content %q: %w", e.ModSlug, err)
	}
	// Check every entry NAME before writing any of them. Names come from the
	// zip index without reading content, so this restores the old whole-object
	// rejection: a stored zip with a traversal-bearing entry - which would
	// zip-slip whoever extracts the pack - is refused before a single byte of
	// it reaches the output.
	for _, f := range zr.File {
		if modpack.IsUnsafeEntryPath(f.Name) {
			return fmt.Errorf("stored content %q has unsafe entry paths", e.ModSlug)
		}
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > maxServerPackEntryBytes {
			return fmt.Errorf("stored content %q entry %q exceeds the size cap", e.ModSlug, f.Name)
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		err = copyPackEntry(zw, f.Name, src, maxServerPackEntryBytes, total)
		src.Close()
		if err != nil {
			return fmt.Errorf("stored content %q entry %q: %w", e.ModSlug, f.Name, err)
		}
	}
	return nil
}

// streamModrinthContent downloads a Modrinth jar to a temp file, verifies its
// hash there, and streams it into the output zip. Verifying against a temp file
// rather than the response keeps the guarantee that unverified bytes never reach
// the client, while still never holding the jar in memory.
func streamModrinthContent(ctx context.Context, zw *zip.Writer, e models.BuildContentEntry, total *packSizeBudget) error {
	if e.ModrinthDownloadURL == "" || e.SHA1 == "" {
		return fmt.Errorf("modrinth content %q missing download URL or sha1", e.ModSlug)
	}
	if e.TargetPath == "" || modpack.IsUnsafeEntryPath(e.TargetPath) {
		return fmt.Errorf("modrinth content %q has an invalid target path", e.ModSlug)
	}
	tmp, err := os.CreateTemp("", "dylaris-modrinth-*")
	if err != nil {
		return err
	}
	defer cleanupTemp(tmp)

	if _, err := services.StreamModrinthJar(ctx, e.ModrinthDownloadURL, tmp, e.SHA1, e.SHA512); err != nil {
		return fmt.Errorf("download %q: %w", e.ModSlug, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := copyPackEntry(zw, e.TargetPath, tmp, maxServerPackEntryBytes, total); err != nil {
		return fmt.Errorf("modrinth content %q: %w", e.ModSlug, err)
	}
	return nil
}

// packSizeBudget tracks the running assembled size against the total cap. A
// pointer is threaded through the per-entry helpers so the cap spans every
// source, exactly as the single local counter did before.
type packSizeBudget struct {
	used  int64
	limit int64
}

// countingWriter passes writes through and records how many bytes have gone to
// the wrapped writer. A streaming handler uses written==0 to tell "failed
// before anything was sent" (a clean error status is still possible) from
// "failed mid-stream" (the status is already committed, only a log is left).
type countingWriter struct {
	w       io.Writer
	written int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// spoolToTemp copies src into a fresh temp file, rewound to the start, bounded
// by limit bytes. Peak memory is io.Copy's buffer, so the object size does not
// enter the heap. The caller cleans the file up with cleanupTemp.
func spoolToTemp(src io.Reader, limit int64) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "dylaris-pack-*")
	if err != nil {
		return nil, 0, err
	}
	// One byte past the cap so a file exactly at the limit is accepted and
	// anything larger is rejected, without measuring the source up front.
	n, err := io.Copy(f, io.LimitReader(src, limit+1))
	if err != nil {
		cleanupTemp(f)
		return nil, 0, err
	}
	if n > limit {
		cleanupTemp(f)
		return nil, 0, fmt.Errorf("stored object exceeds the %d byte cap", limit)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanupTemp(f)
		return nil, 0, err
	}
	return f, n, nil
}

// cleanupTemp closes and removes a temp file, ignoring errors (best-effort
// cleanup on a path where the useful error, if any, was already returned).
func cleanupTemp(f *os.File) {
	if f == nil {
		return
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
}

// copyPackEntry streams src into a new deflate entry named name, enforcing the
// per-entry cap and advancing the total budget as bytes flow - never buffering
// the entry. The per-entry LimitReader guards a source that under-declares its
// size, so a lying zip header cannot smuggle more than the cap.
func copyPackEntry(zw *zip.Writer, name string, src io.Reader, entryCap int64, total *packSizeBudget) error {
	fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return err
	}
	n, err := io.Copy(fw, io.LimitReader(src, entryCap+1))
	if err != nil {
		return err
	}
	if n > entryCap {
		return fmt.Errorf("entry %q exceeds the size cap", name)
	}
	total.used += n
	if total.used > total.limit {
		return fmt.Errorf("server pack exceeds the total size cap")
	}
	return nil
}
