package main

import (
	"encoding/binary"
	"fmt"
	"io"
)

// The Flex recovery images are ~6.9 GB uncompressed, which is past the 4 GB
// ceiling of the original zip format, so the archives are zip64. That is what
// old unzip builds are complaining about when they say "need PK compat v4.5".
//
// Go's archive/zip handles zip64 correctly but requires an io.ReaderAt, which
// means having the whole archive on disk first — exactly the 1.2 GB write we
// are trying to avoid. Since these archives hold a single entry, we can parse
// the local file header ourselves and inflate straight off the wire.

const (
	localHeaderSignature = 0x04034b50
	localHeaderLen       = 30

	flagDataDescriptor = 1 << 3

	methodStore   = 0
	methodDeflate = 8

	zip64ExtraID = 0x0001

	sizeUnknown = -1
)

// LocalEntry describes one entry as announced by its local file header.
type LocalEntry struct {
	Name              string
	Method            uint16
	CRC32             uint32
	CompressedSize    int64 // sizeUnknown when a data descriptor is used
	UncompressedSize  int64 // sizeUnknown when a data descriptor is used
	HasDataDescriptor bool
}

// ReadLocalHeader consumes exactly one local file header from r, leaving r
// positioned at the first byte of compressed data.
func ReadLocalHeader(r io.Reader) (*LocalEntry, error) {
	hdr := make([]byte, localHeaderLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("reading local file header: %w", err)
	}

	if sig := binary.LittleEndian.Uint32(hdr[0:4]); sig != localHeaderSignature {
		return nil, fmt.Errorf("not a zip stream: expected signature %#08x, got %#08x",
			uint32(localHeaderSignature), sig)
	}

	flags := binary.LittleEndian.Uint16(hdr[6:8])
	e := &LocalEntry{
		Method:            binary.LittleEndian.Uint16(hdr[8:10]),
		CRC32:             binary.LittleEndian.Uint32(hdr[14:18]),
		CompressedSize:    int64(binary.LittleEndian.Uint32(hdr[18:22])),
		UncompressedSize:  int64(binary.LittleEndian.Uint32(hdr[22:26])),
		HasDataDescriptor: flags&flagDataDescriptor != 0,
	}

	nameLen := int(binary.LittleEndian.Uint16(hdr[26:28]))
	extraLen := int(binary.LittleEndian.Uint16(hdr[28:30]))

	name := make([]byte, nameLen)
	if _, err := io.ReadFull(r, name); err != nil {
		return nil, fmt.Errorf("reading entry name: %w", err)
	}
	e.Name = string(name)

	extra := make([]byte, extraLen)
	if _, err := io.ReadFull(r, extra); err != nil {
		return nil, fmt.Errorf("reading extra field: %w", err)
	}

	// 0xFFFFFFFF in either size field means the real value lives in the zip64
	// extra field. In a local header the zip64 record always carries both
	// sizes, uncompressed first.
	if e.CompressedSize == 0xFFFFFFFF || e.UncompressedSize == 0xFFFFFFFF {
		un, comp, ok := parseZip64Extra(extra)
		if !ok {
			return nil, fmt.Errorf("entry %q declares zip64 sizes but has no usable zip64 extra field", e.Name)
		}
		e.UncompressedSize = un
		e.CompressedSize = comp
	}

	// With a data descriptor the sizes are written after the data, so the
	// header values are meaningless.
	if e.HasDataDescriptor {
		e.CompressedSize = sizeUnknown
		e.UncompressedSize = sizeUnknown
		e.CRC32 = 0
	}

	return e, nil
}

// parseZip64Extra walks the extra field looking for the zip64 record.
func parseZip64Extra(extra []byte) (uncompressed, compressed int64, ok bool) {
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra[0:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		extra = extra[4:]

		if size > len(extra) {
			return 0, 0, false
		}
		if id == zip64ExtraID && size >= 16 {
			return int64(binary.LittleEndian.Uint64(extra[0:8])),
				int64(binary.LittleEndian.Uint64(extra[8:16])),
				true
		}
		extra = extra[size:]
	}
	return 0, 0, false
}

// Decompressor wraps the entry body in whatever reader its method requires.
// The returned Closer is nil for stored entries.
func (e *LocalEntry) Describe() string {
	switch e.Method {
	case methodStore:
		return "stored"
	case methodDeflate:
		return "deflate"
	default:
		return fmt.Sprintf("method %d", e.Method)
	}
}
