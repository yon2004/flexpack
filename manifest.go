package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultManifestURL is Google's recovery config for ChromeOS Flex (board "reven").
// It is a plain-text file of blank-line-separated stanzas, one per available image.
const DefaultManifestURL = "https://dl.google.com/dl/edgedl/chromeos/recovery/cloudready_recovery2.conf"

// Image is one stanza from the recovery manifest.
type Image struct {
	Name        string // "ChromeOS Flex"
	Version     string // "15117.112.0"
	Channel     string // "STABLE" / "DEV" / "LTS" / "LTC"
	File        string // uncompressed .bin filename
	URL         string // .bin.zip download URL
	MD5         string // md5 of the .zip
	SHA1        string // sha1 of the .zip
	ZipFileSize int64  // bytes of the .zip
	FileSize    int64  // bytes of the extracted .bin
}

func (im Image) String() string {
	return fmt.Sprintf("%-7s %-14s %s (%s zip, %s image)",
		im.Channel, im.Version, im.File,
		humanBytes(im.ZipFileSize), humanBytes(im.FileSize))
}

// FetchManifest downloads and parses the recovery manifest.
func FetchManifest(url string) ([]Image, error) {
	if url == "" {
		url = DefaultManifestURL
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest returned HTTP %d", resp.StatusCode)
	}

	// Manifests are small (single-digit KB); cap generously to avoid surprises.
	body := io.LimitReader(resp.Body, 4<<20)
	return ParseManifest(body)
}

// ParseManifest reads the key=value stanza format.
//
// The file opens with a global stanza (recovery_tool_version and friends) that
// carries no url= key. Rather than special-casing position, we keep any stanza
// that has both url and file — that is what makes a stanza an image.
func ParseManifest(r io.Reader) ([]Image, error) {
	var (
		images  []Image
		current = map[string]string{}
	)

	flush := func() {
		if current["url"] != "" && current["file"] != "" {
			images = append(images, imageFromFields(current))
		}
		current = map[string]string{}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			flush()
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue // tolerate junk rather than failing the whole parse
		}
		current[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	flush()

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("manifest contained no images with a url= and file= key")
	}
	return images, nil
}

func imageFromFields(f map[string]string) Image {
	atoi := func(s string) int64 {
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	return Image{
		Name:        f["name"],
		Version:     f["version"],
		Channel:     strings.ToUpper(f["channel"]),
		File:        f["file"],
		URL:         f["url"],
		MD5:         strings.ToLower(f["md5"]),
		SHA1:        strings.ToLower(f["sha1"]),
		ZipFileSize: atoi(f["zipfilesize"]),
		FileSize:    atoi(f["filesize"]),
	}
}

// SelectChannel picks the image for a channel, case-insensitively.
func SelectChannel(images []Image, channel string) (Image, error) {
	want := strings.ToUpper(strings.TrimSpace(channel))
	for _, im := range images {
		if im.Channel == want {
			return im, nil
		}
	}

	var have []string
	for _, im := range images {
		have = append(have, im.Channel)
	}
	return Image{}, fmt.Errorf("no image on channel %q; manifest offers: %s",
		channel, strings.Join(have, ", "))
}
