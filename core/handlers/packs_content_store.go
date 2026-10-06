package handlers

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"
)

// storedUpload carries what the DB write needs after the object is in storage.
type storedUpload struct {
	key     string
	version string
	size    int64
	md5     string // md5 of the STORED object (drives version/key)
	// inner hashes are of the RAW upload, used for Modrinth auto-link.
	innerSha1   string
	innerSha512 string
}

// uploadError is a handler-facing error carrying the status + message to send.
type uploadError struct {
	status int
	msg    string
}

func upErr(status int, msg string) *uploadError { return &uploadError{status: status, msg: msg} }

// storeUploadedContent turns a multipart upload into a stored content zip and
// returns its metadata. It splits by the largest case:
//
//   - a pre-built content .zip is STORED AS-IS, and that is the case that can be
//     large, so it is validated over the seekable upload, hashed by streaming,
//     and streamed to storage - never read into memory. Its stored hashes ARE
//     the raw hashes (the object is the upload), so one pass yields all four.
//   - a raw .jar or a single config/resourcepack file is WRAPPED into a content
//     zip. A resourcepack can be as large as a stored zip, so this path streams
//     too: upload -> zip writer -> temp file -> PutStream.
func (h *PacksHandler) storeUploadedContent(
	ctx context.Context,
	prov modpack.ModpackStorageProvider,
	f multipart.File,
	hdr *multipart.FileHeader,
	fileName, contentType, userID, slug string,
) (storedUpload, *uploadError) {
	lower := strings.ToLower(fileName)

	if strings.HasSuffix(lower, ".zip") && contentType == models.ContentTypeMod {
		return h.streamStoredZip(ctx, prov, f, hdr.Size, userID, slug)
	}

	return h.streamWrappedUpload(ctx, prov, f, fileName, contentType, userID, slug)
}

// streamWrappedUpload wraps a raw .jar or a single config/resourcepack file into
// a content zip and stores it, through a temp file rather than memory. It used
// to read the upload and build the zip in RAM, two full copies of a file the
// upload cap lets reach 1 GiB, with nothing bounding how many ran at once. The
// raw hashes (for Modrinth auto-link) are taken as the upload is read, the
// stored hashes as the zip is written.
func (h *PacksHandler) streamWrappedUpload(
	ctx context.Context,
	prov modpack.ModpackStorageProvider,
	f multipart.File,
	fileName, contentType, userID, slug string,
) (storedUpload, *uploadError) {
	tmp, err := os.CreateTemp("", "dylaris-pack-*")
	if err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Failed to read upload")
	}
	defer cleanupTemp(tmp)

	in1, in5 := sha1.New(), sha512.New()
	outMD5, out1 := md5.New(), sha1.New()
	src := io.TeeReader(f, io.MultiWriter(in1, in5))
	dst := &countingWriter{w: io.MultiWriter(tmp, outMD5, out1)}
	if strings.HasSuffix(strings.ToLower(fileName), ".jar") {
		err = modpack.WriteJarContentZip(dst, fileName, src)
	} else {
		err = modpack.WriteContentZip(dst, targetPathFor(contentType, fileName), src)
	}
	if err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Failed to wrap file")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Failed to read upload")
	}
	storedSHA1 := hex.EncodeToString(out1.Sum(nil))
	key, version := uploadKey(userID, slug, storedSHA1)
	if err := prov.PutStream(ctx, key, tmp, dst.written); err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Storage put failed")
	}
	return storedUpload{
		key: key, version: version, size: dst.written,
		md5:         hex.EncodeToString(outMD5.Sum(nil)),
		innerSha1:   hex.EncodeToString(in1.Sum(nil)),
		innerSha512: hex.EncodeToString(in5.Sum(nil)),
	}, nil
}

// streamStoredZip validates, hashes and stores a pre-built content zip without
// ever holding it in memory. The upload is seekable (a multipart.File spills to
// disk past the small parse buffer), so it is read three times - validate, hash,
// store - at a fixed memory cost regardless of size.
func (h *PacksHandler) streamStoredZip(
	ctx context.Context,
	prov modpack.ModpackStorageProvider,
	f multipart.File,
	size int64,
	userID, slug string,
) (storedUpload, *uploadError) {
	if err := validateStoredModZip(f, size); err != nil {
		return storedUpload{}, upErr(http.StatusBadRequest, err.Error())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Failed to read upload")
	}

	mh, s1, s5 := md5.New(), sha1.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(mh, s1, s5), f); err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Failed to read upload")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Failed to read upload")
	}
	// The object IS the upload, so its stored hashes and its inner (raw) hashes
	// are the same bytes.
	storedSHA1 := hex.EncodeToString(s1.Sum(nil))
	key, version := uploadKey(userID, slug, storedSHA1)
	if err := prov.PutStream(ctx, key, f, size); err != nil {
		return storedUpload{}, upErr(http.StatusInternalServerError, "Storage put failed")
	}
	return storedUpload{
		key: key, version: version, size: size,
		md5:         hex.EncodeToString(mh.Sum(nil)),
		innerSha1:   storedSHA1,
		innerSha512: hex.EncodeToString(s5.Sum(nil)),
	}, nil
}

// uploadKey derives the storage key and version from the stored object's sha1,
// the one place both are built so the two paths cannot drift.
func uploadKey(userID, slug, storedSHA1 string) (key, version string) {
	version = "u-" + storedSHA1[:8]
	key = "packs/" + userID + "/mods/" + slug + "/" + slug + "-" + version + ".zip"
	return key, version
}
