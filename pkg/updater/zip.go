package updater

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// installFromZip extracts the anchored binary from a zip archive and swaps it
// into place. The windows release ships zip, and archive/zip needs random
// access, so the archive is buffered in memory rather than streamed — bounded
// by maxBytes exactly like the tar path, and the digest is still computed over
// the bytes as they arrive.
func installFromZip(body io.Reader, hasher hash.Hash, tmp *os.File, tmpPath, dst, wantSum string, maxBytes int64) error {
	var buf bytes.Buffer
	n, err := io.CopyN(&buf, body, maxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return abortStaging(tmp, tmpPath, fmt.Errorf("read zip: %w", err))
	}
	if n > maxBytes {
		return abortStaging(tmp, tmpPath, fmt.Errorf("archive exceeds the %d byte limit", maxBytes))
	}

	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if gotSum != strings.ToLower(wantSum) {
		return abortStaging(tmp, tmpPath, fmt.Errorf("checksum mismatch: want %s got %s", wantSum, gotSum))
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return abortStaging(tmp, tmpPath, fmt.Errorf("zip: %w", err))
	}

	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isAnchoredBinary(f.Name) {
			continue
		}
		if int64(f.UncompressedSize64) > maxBytes {
			return abortStaging(tmp, tmpPath,
				fmt.Errorf("zip entry %q declares %d bytes, over the %d limit", f.Name, f.UncompressedSize64, maxBytes))
		}
		rc, err := f.Open()
		if err != nil {
			return abortStaging(tmp, tmpPath, fmt.Errorf("open zip entry: %w", err))
		}
		written, err := io.CopyN(tmp, rc, maxBytes+1)
		if closeErr := rc.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return abortStaging(tmp, tmpPath, fmt.Errorf("write tmp: %w", err))
		}
		if written > maxBytes {
			return abortStaging(tmp, tmpPath, fmt.Errorf("zip payload exceeds the %d byte limit", maxBytes))
		}
		if err := tmp.Close(); err != nil {
			if rmErr := os.Remove(tmpPath); rmErr != nil {
				return fmt.Errorf("close tmp: %w (and %s could not be removed: %v)", err, tmpPath, rmErr)
			}
			return fmt.Errorf("close tmp: %w", err)
		}
		return swapInPlace(tmpPath, dst)
	}

	return abortStaging(tmp, tmpPath, errors.New("anchored binary not found in zip"))
}
