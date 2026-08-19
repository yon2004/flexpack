package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"unicode/utf16"
)

// Minimal GPT reader — enough to find where a partition starts and how big it
// is. ChromeOS uses non-standard partition type GUIDs, so we locate partitions
// by their GPT name ("STATE") or by index, never by type.

const (
	gptHeaderLBA = 1
	gptSignature = "EFI PART"
)

type Partition struct {
	Index  int
	Name   string
	Start  int64 // bytes
	Length int64 // bytes
}

func (p Partition) String() string {
	return fmt.Sprintf("%2d  %-12s start=%-12d size=%s", p.Index, p.Name, p.Start, humanBytes(p.Length))
}

// ReadGPT parses the primary GPT. sectorSize is usually 512.
func ReadGPT(f *os.File, sectorSize int64) ([]Partition, error) {
	hdr := make([]byte, 92)
	if _, err := f.ReadAt(hdr, gptHeaderLBA*sectorSize); err != nil {
		return nil, fmt.Errorf("reading GPT header: %w", err)
	}
	if string(hdr[0:8]) != gptSignature {
		return nil, fmt.Errorf("no GPT found at LBA 1 (is this a ChromeOS disk image?)")
	}

	entryLBA := int64(binary.LittleEndian.Uint64(hdr[72:80]))
	numEntries := int64(binary.LittleEndian.Uint32(hdr[80:84]))
	entrySize := int64(binary.LittleEndian.Uint32(hdr[84:88]))

	if entrySize < 128 || numEntries <= 0 || numEntries > 1024 {
		return nil, fmt.Errorf("GPT header is implausible: %d entries of %d bytes", numEntries, entrySize)
	}

	table := make([]byte, numEntries*entrySize)
	if _, err := f.ReadAt(table, entryLBA*sectorSize); err != nil {
		return nil, fmt.Errorf("reading GPT entries: %w", err)
	}

	var parts []Partition
	for i := int64(0); i < numEntries; i++ {
		e := table[i*entrySize : (i+1)*entrySize]

		// All-zero type GUID means the slot is unused.
		empty := true
		for _, b := range e[0:16] {
			if b != 0 {
				empty = false
				break
			}
		}
		if empty {
			continue
		}

		first := int64(binary.LittleEndian.Uint64(e[32:40]))
		last := int64(binary.LittleEndian.Uint64(e[40:48]))

		parts = append(parts, Partition{
			Index:  int(i) + 1,
			Name:   decodeUTF16Name(e[56:128]),
			Start:  first * sectorSize,
			Length: (last - first + 1) * sectorSize,
		})
	}
	return parts, nil
}

func decodeUTF16Name(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// FindPartition looks up a partition by GPT name, case-sensitively.
func FindPartition(parts []Partition, name string) (Partition, bool) {
	for _, p := range parts {
		if p.Name == name {
			return p, true
		}
	}
	return Partition{}, false
}
