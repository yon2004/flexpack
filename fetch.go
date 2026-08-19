package main

import (
	"compress/flate"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fetch streams image.URL straight into outPath, inflating on the fly.
//
// The naive approach writes the 1.2 GB archive, reads it back, then writes the
// 6.9 GB image — roughly 16 GB of disk traffic. This does one pass: bytes come
// off the socket, through the hashers, through inflate, and land in the output
// file. Nothing else touches the disk.
func Fetch(image Image, outPath string, verify bool) error {
	if outPath == "" {
		outPath = image.File
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil && filepath.Dir(outPath) != "." {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write to a temp file and rename on success, so an interrupted run never
	// leaves something that looks like a finished image.
	tmpPath := outPath + ".partial"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmpPath, err)
	}
	defer func() {
		out.Close()
		os.Remove(tmpPath) // no-op once renamed
	}()

	client := &http.Client{Timeout: 0} // large download; rely on the server
	resp, err := client.Get(image.URL)
	if err != nil {
		return fmt.Errorf("starting download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	sha := sha1.New()
	m5 := md5.New()
	counter := &countingWriter{}

	prog := newProgress(image.ZipFileSize)
	defer prog.done()
	counter.onWrite = prog.update

	// Everything pulled from `wire` is hashed and counted exactly once.
	wire := io.TeeReader(resp.Body, io.MultiWriter(sha, m5, counter))

	entry, err := ReadLocalHeader(wire)
	if err != nil {
		return err
	}

	var body io.Reader = wire
	if entry.CompressedSize != sizeUnknown {
		// Cap the inflater so it cannot wander into the central directory.
		body = io.LimitReader(wire, entry.CompressedSize)
	}

	var payload io.Reader
	switch entry.Method {
	case methodStore:
		payload = body
	case methodDeflate:
		fr := flate.NewReader(body)
		defer fr.Close()
		payload = fr
	default:
		return fmt.Errorf("entry %q uses unsupported compression (%s); "+
			"this tool handles stored and deflate only", entry.Name, entry.Describe())
	}

	written, err := io.Copy(out, payload)
	if err != nil {
		return fmt.Errorf("inflating image after %s: %w", humanBytes(written), err)
	}

	// Drain whatever follows the entry (data descriptor, central directory,
	// end-of-central-directory) so the hashes cover the whole archive.
	if _, err := io.Copy(io.Discard, wire); err != nil {
		return fmt.Errorf("draining archive tail: %w", err)
	}
	prog.done()

	if err := out.Sync(); err != nil {
		return fmt.Errorf("flushing image: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing image: %w", err)
	}

	if verify {
		if err := checkIntegrity(image, written, counter.n, sha, m5); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "verified: sha1, md5, zip size and image size all match the manifest")
	}

	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("renaming into place: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%s)\n", outPath, humanBytes(written))
	return nil
}

func checkIntegrity(image Image, written, read int64, sha, m5 hash.Hash) error {
	var problems []string

	if image.FileSize > 0 && written != image.FileSize {
		problems = append(problems, fmt.Sprintf(
			"image size %d, manifest says %d", written, image.FileSize))
	}
	if image.ZipFileSize > 0 && read != image.ZipFileSize {
		problems = append(problems, fmt.Sprintf(
			"archive size %d, manifest says %d", read, image.ZipFileSize))
	}
	if image.SHA1 != "" {
		if got := hex.EncodeToString(sha.Sum(nil)); got != image.SHA1 {
			problems = append(problems, fmt.Sprintf("sha1 %s, manifest says %s", got, image.SHA1))
		}
	}
	if image.MD5 != "" {
		if got := hex.EncodeToString(m5.Sum(nil)); got != image.MD5 {
			problems = append(problems, fmt.Sprintf("md5 %s, manifest says %s", got, image.MD5))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("integrity check failed:\n  - %s\n"+
			"the download is not trustworthy; re-run, or pass --no-verify to keep it anyway",
			strings.Join(problems, "\n  - "))
	}
	return nil
}

type countingWriter struct {
	n       int64
	onWrite func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if c.onWrite != nil {
		c.onWrite(c.n)
	}
	return len(p), nil
}

// progress prints a single rewriting status line to stderr, throttled so it
// does not flood a log when output is redirected.
type progress struct {
	total    int64
	start    time.Time
	lastDraw time.Time
	finished bool
}

func newProgress(total int64) *progress {
	return &progress{total: total, start: time.Now()}
}

func (p *progress) update(n int64) {
	now := time.Now()
	if p.finished || now.Sub(p.lastDraw) < 250*time.Millisecond {
		return
	}
	p.lastDraw = now

	elapsed := now.Sub(p.start).Seconds()
	rate := ""
	if elapsed > 0.5 {
		rate = fmt.Sprintf(" @ %s/s", humanBytes(int64(float64(n)/elapsed)))
	}

	if p.total > 0 {
		fmt.Fprintf(os.Stderr, "\r  %s / %s (%.1f%%)%s      ",
			humanBytes(n), humanBytes(p.total), 100*float64(n)/float64(p.total), rate)
	} else {
		fmt.Fprintf(os.Stderr, "\r  %s%s      ", humanBytes(n), rate)
	}
}

func (p *progress) done() {
	if !p.finished {
		p.finished = true
		fmt.Fprintln(os.Stderr, "\r                                                            \r  download complete")
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
