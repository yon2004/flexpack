package main

// A deliberately minimal ext4 writer.
//
// It does exactly one job: create a directory and write a small file into an
// existing, cleanly-unmounted ext4 filesystem. It is not a general filesystem
// implementation and will refuse anything it cannot do correctly rather than
// guess — an ext4 writer that guesses produces a filesystem that mounts once
// and then eats itself, which is precisely the failure we are avoiding.
//
// Scope, and why each limit is safe here:
//
//   - One block per file. Config files are well under 4 KiB.
//   - Single-entry, depth-0 extent tree. Follows from one block.
//   - No hashed (htree) directories. Refused explicitly, see addDirent.
//   - No journal replay or transaction. The filesystem is offline and we are
//     the only writer, so there is nothing to recover. This is what debugfs
//     does too.
//   - Allocation only in groups already initialised by mkfs. Avoids having to
//     synthesise bitmaps for BLOCK_UNINIT/INODE_UNINIT groups.
//
// Everything is validated with `e2fsck -fn`. If e2fsck is not clean, this
// code is wrong, no matter what it returns.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"
)

const (
	superblockOffset = 1024
	extMagic         = 0xEF53
	rootInode        = 2

	// s_feature_incompat
	featIncompat64Bit    = 0x0080
	featIncompatExtents  = 0x0040
	featIncompatFileType = 0x0002

	// s_feature_ro_compat
	featROCompatMetadataCsum = 0x0400
	featROCompatGDTCsum      = 0x0010

	// s_feature_compat
	featCompatDirIndex = 0x0020

	// bg_flags
	bgInodeUninit = 0x0001
	bgBlockUninit = 0x0002

	// i_flags
	inodeFlagExtents = 0x00080000
	inodeFlagIndex   = 0x00001000 // htree directory

	// i_mode
	modeDir  = 0x4000
	modeFile = 0x8000

	fileTypeRegular = 1
	fileTypeDir     = 2

	direntTailLen  = 12
	direntTailMark = 0xDE // fake file_type marking the checksum tail

	extentHeaderMagic = 0xF30A
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c matches e2fsprogs' ext2fs_crc32c_le: a raw CRC-32C update with no
// final inversion, so calls chain. Go's crc32.Update inverts on the way in and
// out, hence the double complement.
func crc32c(seed uint32, p []byte) uint32 {
	return ^crc32.Update(^seed, castagnoli, p)
}

func le16(b []byte) uint16     { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32     { return binary.LittleEndian.Uint32(b) }
func put16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func put32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

// FS is an open ext4 filesystem inside a file, starting at Offset bytes.
type FS struct {
	f      *os.File
	offset int64

	sb []byte // live copy of the primary superblock

	blockSize      int64
	blocksPerGroup uint32
	inodesPerGroup uint32
	inodeSize      uint32
	firstDataBlock uint32
	descSize       uint32
	groups         uint32
	csumSeed       uint32
	metadataCsum   bool
	is64Bit        bool
	gdtBlock       int64 // block number where group descriptors start
}

// OpenFS reads and validates the superblock.
func OpenFS(f *os.File, offset int64) (*FS, error) {
	sb := make([]byte, 1024)
	if _, err := f.ReadAt(sb, offset+superblockOffset); err != nil {
		return nil, fmt.Errorf("reading superblock: %w", err)
	}
	if le16(sb[0x38:]) != extMagic {
		return nil, fmt.Errorf("no ext2/3/4 superblock at offset %d (magic %#04x)", offset, le16(sb[0x38:]))
	}

	fs := &FS{f: f, offset: offset, sb: sb}
	fs.blockSize = int64(1024) << le32(sb[0x18:])
	fs.blocksPerGroup = le32(sb[0x20:])
	fs.inodesPerGroup = le32(sb[0x28:])
	fs.inodeSize = uint32(le16(sb[0x58:]))
	fs.firstDataBlock = le32(sb[0x14:])

	incompat := le32(sb[0x60:])
	roCompat := le32(sb[0x64:])
	fs.is64Bit = incompat&featIncompat64Bit != 0
	fs.metadataCsum = roCompat&featROCompatMetadataCsum != 0

	if incompat&featIncompatExtents == 0 {
		return nil, fmt.Errorf("filesystem has no extents feature; this writer only emits extent-mapped inodes")
	}
	if incompat&featIncompatFileType == 0 {
		return nil, fmt.Errorf("filesystem has no filetype feature; unsupported")
	}

	fs.descSize = 32
	if fs.is64Bit {
		if d := uint32(le16(sb[0xFE:])); d > 0 {
			fs.descSize = d
		} else {
			fs.descSize = 64
		}
	}

	blocksCount := uint64(le32(sb[0x04:]))
	if fs.is64Bit {
		blocksCount |= uint64(le32(sb[0x150:])) << 32
	}
	fs.groups = uint32((blocksCount - uint64(fs.firstDataBlock) + uint64(fs.blocksPerGroup) - 1) / uint64(fs.blocksPerGroup))

	// Metadata checksums are seeded from the UUID unless the filesystem
	// carries an explicit seed.
	fs.csumSeed = crc32c(^uint32(0), sb[0x68:0x78])

	fs.gdtBlock = int64(fs.firstDataBlock) + 1

	return fs, nil
}

func (fs *FS) Label() string {
	b := fs.sb[0x78:0x88]
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func (fs *FS) readBlock(n int64) ([]byte, error) {
	buf := make([]byte, fs.blockSize)
	_, err := fs.f.ReadAt(buf, fs.offset+n*fs.blockSize)
	return buf, err
}

func (fs *FS) writeBlock(n int64, b []byte) error {
	if int64(len(b)) != fs.blockSize {
		return fmt.Errorf("internal: block write of %d bytes, expected %d", len(b), fs.blockSize)
	}
	_, err := fs.f.WriteAt(b, fs.offset+n*fs.blockSize)
	return err
}

func (fs *FS) readAt(p []byte, off int64) error {
	_, err := fs.f.ReadAt(p, fs.offset+off)
	return err
}

func (fs *FS) writeAt(p []byte, off int64) error {
	_, err := fs.f.WriteAt(p, fs.offset+off)
	return err
}

// ---------------------------------------------------------------- descriptors

func (fs *FS) descOffset(group uint32) int64 {
	return fs.gdtBlock*fs.blockSize + int64(group)*int64(fs.descSize)
}

func (fs *FS) readDesc(group uint32) ([]byte, error) {
	d := make([]byte, fs.descSize)
	return d, fs.readAt(d, fs.descOffset(group))
}

func (fs *FS) writeDesc(group uint32, d []byte) error {
	fs.setDescCsum(group, d)
	return fs.writeAt(d, fs.descOffset(group))
}

// setDescCsum recomputes bg_checksum over the group number and the descriptor
// with the checksum field itself excluded.
func (fs *FS) setDescCsum(group uint32, d []byte) {
	if !fs.metadataCsum && le32(fs.sb[0x64:])&featROCompatGDTCsum == 0 {
		return
	}
	const csumOff = 0x1E
	put16(d[csumOff:], 0)

	var gnum [4]byte
	put32(gnum[:], group)

	// The metadata_csum path zeroes bg_checksum and hashes the descriptor in
	// one contiguous run. Only the legacy crc16 GDT_CSUM path skips the field.
	crc := crc32c(fs.csumSeed, gnum[:])
	crc = crc32c(crc, d)
	put16(d[csumOff:], uint16(crc&0xFFFF))
}

func descBlockBitmap(d []byte, is64 bool) int64 {
	v := int64(le32(d[0x00:]))
	if is64 && len(d) >= 0x24 {
		v |= int64(le32(d[0x20:])) << 32
	}
	return v
}

func descInodeBitmap(d []byte, is64 bool) int64 {
	v := int64(le32(d[0x04:]))
	if is64 && len(d) >= 0x28 {
		v |= int64(le32(d[0x24:])) << 32
	}
	return v
}

func descInodeTable(d []byte, is64 bool) int64 {
	v := int64(le32(d[0x08:]))
	if is64 && len(d) >= 0x2C {
		v |= int64(le32(d[0x28:])) << 32
	}
	return v
}

func descFreeBlocks(d []byte, is64 bool) uint32 {
	v := uint32(le16(d[0x0C:]))
	if is64 && len(d) >= 0x2E {
		v |= uint32(le16(d[0x2C:])) << 16
	}
	return v
}

func setDescFreeBlocks(d []byte, is64 bool, v uint32) {
	put16(d[0x0C:], uint16(v))
	if is64 && len(d) >= 0x2E {
		put16(d[0x2C:], uint16(v>>16))
	}
}

func descFreeInodes(d []byte, is64 bool) uint32 {
	v := uint32(le16(d[0x0E:]))
	if is64 && len(d) >= 0x30 {
		v |= uint32(le16(d[0x2E:])) << 16
	}
	return v
}

func setDescFreeInodes(d []byte, is64 bool, v uint32) {
	put16(d[0x0E:], uint16(v))
	if is64 && len(d) >= 0x30 {
		put16(d[0x2E:], uint16(v>>16))
	}
}

func descUsedDirs(d []byte, is64 bool) uint32 {
	v := uint32(le16(d[0x10:]))
	if is64 && len(d) >= 0x32 {
		v |= uint32(le16(d[0x30:])) << 16
	}
	return v
}

func setDescUsedDirs(d []byte, is64 bool, v uint32) {
	put16(d[0x10:], uint16(v))
	if is64 && len(d) >= 0x32 {
		put16(d[0x30:], uint16(v>>16))
	}
}

// setDescItableUnused clears bg_itable_unused. Zero always parses: it tells
// the kernel every inode table entry must be scanned, which is conservative
// and correct once we have written into the group.
func setDescItableUnused(d []byte, is64 bool) {
	put16(d[0x1C:], 0)
	if is64 && len(d) >= 0x34 {
		put16(d[0x32:], 0)
	}
}

// -------------------------------------------------------------------- bitmaps

func (fs *FS) bitmapCsumSet(d []byte, bitmap []byte, size int, loOff, hiOff int) {
	if !fs.metadataCsum {
		return
	}
	crc := crc32c(fs.csumSeed, bitmap[:size])
	put16(d[loOff:], uint16(crc&0xFFFF))
	if uint32(len(d)) > uint32(hiOff)+2 {
		put16(d[hiOff:], uint16(crc>>16))
	}
}

func bitSet(bm []byte, i uint32) bool { return bm[i/8]&(1<<(i%8)) != 0 }
func setBit(bm []byte, i uint32)      { bm[i/8] |= 1 << (i % 8) }

// ------------------------------------------------------------------- inodes

type inode struct {
	num uint32
	raw []byte
}

func (fs *FS) inodeOffset(num uint32) (int64, error) {
	if num == 0 {
		return 0, fmt.Errorf("inode 0 is not valid")
	}
	group := (num - 1) / fs.inodesPerGroup
	index := (num - 1) % fs.inodesPerGroup
	if group >= fs.groups {
		return 0, fmt.Errorf("inode %d is past the end of the filesystem", num)
	}
	d, err := fs.readDesc(group)
	if err != nil {
		return 0, err
	}
	table := descInodeTable(d, fs.is64Bit)
	return table*fs.blockSize + int64(index)*int64(fs.inodeSize), nil
}

func (fs *FS) readInode(num uint32) (*inode, error) {
	off, err := fs.inodeOffset(num)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, fs.inodeSize)
	if err := fs.readAt(raw, off); err != nil {
		return nil, fmt.Errorf("reading inode %d: %w", num, err)
	}
	return &inode{num: num, raw: raw}, nil
}

func (fs *FS) writeInode(in *inode) error {
	fs.setInodeCsum(in)
	off, err := fs.inodeOffset(in.num)
	if err != nil {
		return err
	}
	return fs.writeAt(in.raw, off)
}

// setInodeCsum computes i_checksum_lo/hi over the inode number, generation and
// the inode body with the checksum fields zeroed.
func (fs *FS) setInodeCsum(in *inode) {
	if !fs.metadataCsum {
		return
	}
	const (
		csumLoOff = 0x7C
		csumHiOff = 0x82
		extraOff  = 0x80
	)
	hasHi := fs.inodeSize > 128 && le16(in.raw[extraOff:]) >= 4

	put16(in.raw[csumLoOff:], 0)
	if hasHi {
		put16(in.raw[csumHiOff:], 0)
	}

	var num, gen [4]byte
	put32(num[:], in.num)
	copy(gen[:], in.raw[0x64:0x68]) // i_generation, already little-endian

	crc := crc32c(fs.csumSeed, num[:])
	crc = crc32c(crc, gen[:])
	crc = crc32c(crc, in.raw)

	put16(in.raw[csumLoOff:], uint16(crc&0xFFFF))
	if hasHi {
		put16(in.raw[csumHiOff:], uint16(crc>>16))
	}
}

func (in *inode) mode() uint16      { return le16(in.raw[0x00:]) }
func (in *inode) isDir() bool       { return in.mode()&0xF000 == modeDir }
func (in *inode) flags() uint32     { return le32(in.raw[0x20:]) }
func (in *inode) links() uint16     { return le16(in.raw[0x1A:]) }
func (in *inode) setLinks(n uint16) { put16(in.raw[0x1A:], n) }

func (in *inode) size() uint64 {
	return uint64(le32(in.raw[0x04:])) | uint64(le32(in.raw[0x6C:]))<<32
}

func (in *inode) setSize(n uint64) {
	put32(in.raw[0x04:], uint32(n))
	put32(in.raw[0x6C:], uint32(n>>32))
}

// firstExtentBlock returns the physical block of a depth-0, single-extent
// inode. Anything more complex is refused: we only ever read directory blocks
// small enough to live in one extent.
func (in *inode) firstExtentBlock() (int64, error) {
	ib := in.raw[0x28:0x64]
	if le16(ib[0:]) != extentHeaderMagic {
		return 0, fmt.Errorf("inode %d does not use an extent tree", in.num)
	}
	if depth := le16(ib[6:]); depth != 0 {
		return 0, fmt.Errorf("inode %d has a depth-%d extent tree; only depth 0 is supported", in.num, depth)
	}
	if entries := le16(ib[2:]); entries != 1 {
		return 0, fmt.Errorf("inode %d has %d extents; only single-extent inodes are supported", in.num, entries)
	}
	e := ib[12:24]
	return int64(le32(e[8:])) | int64(le16(e[6:]))<<32, nil
}

// setSingleExtent writes a depth-0 extent tree mapping logical block 0 to blk.
func (in *inode) setSingleExtent(blk int64) {
	ib := in.raw[0x28:0x64]
	for i := range ib {
		ib[i] = 0
	}
	put16(ib[0:], extentHeaderMagic)
	put16(ib[2:], 1) // entries
	put16(ib[4:], 4) // max entries that fit in i_block
	put16(ib[6:], 0) // depth

	e := ib[12:]
	put32(e[0:], 0)                      // logical block
	put16(e[4:], 1)                      // length
	put16(e[6:], uint16(blk>>32))        // physical hi
	put32(e[8:], uint32(blk&0xFFFFFFFF)) // physical lo
}

// ---------------------------------------------------------------- allocation

// pickGroup finds a group that mkfs already initialised and that has room.
// Skipping UNINIT groups means we never have to synthesise a bitmap.
func (fs *FS) pickGroup(needInode, needBlock bool) (uint32, []byte, error) {
	for g := uint32(0); g < fs.groups; g++ {
		d, err := fs.readDesc(g)
		if err != nil {
			return 0, nil, err
		}
		flags := le16(d[0x12:])
		if needInode && flags&bgInodeUninit != 0 {
			continue
		}
		if needBlock && flags&bgBlockUninit != 0 {
			continue
		}
		if needInode && descFreeInodes(d, fs.is64Bit) == 0 {
			continue
		}
		if needBlock && descFreeBlocks(d, fs.is64Bit) == 0 {
			continue
		}
		return g, d, nil
	}
	return 0, nil, fmt.Errorf("no initialised block group has both a free inode and a free block; " +
		"this writer does not initialise uninitialised groups")
}

// allocInode reserves one inode in group g and returns its absolute number.
func (fs *FS) allocInode(g uint32, d []byte, isDir bool) (uint32, error) {
	bmBlock := descInodeBitmap(d, fs.is64Bit)
	bm, err := fs.readBlock(bmBlock)
	if err != nil {
		return 0, fmt.Errorf("reading inode bitmap for group %d: %w", g, err)
	}

	firstIno := le32(fs.sb[0x54:])
	var idx uint32 = 0
	found := false
	for i := uint32(0); i < fs.inodesPerGroup; i++ {
		abs := g*fs.inodesPerGroup + i + 1
		if abs < firstIno {
			continue // reserved inodes
		}
		if !bitSet(bm, i) {
			idx, found = i, true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("group %d reported free inodes but the bitmap is full", g)
	}

	setBit(bm, idx)
	bmSize := int((fs.inodesPerGroup + 7) / 8)
	fs.bitmapCsumSet(d, bm, bmSize, 0x1A, 0x3A)
	if err := fs.writeBlock(bmBlock, bm); err != nil {
		return 0, err
	}

	setDescFreeInodes(d, fs.is64Bit, descFreeInodes(d, fs.is64Bit)-1)
	if isDir {
		setDescUsedDirs(d, fs.is64Bit, descUsedDirs(d, fs.is64Bit)+1)
	}
	setDescItableUnused(d, fs.is64Bit)
	put16(d[0x12:], le16(d[0x12:])&^bgInodeUninit)

	put32(fs.sb[0x10:], le32(fs.sb[0x10:])-1) // s_free_inodes_count

	return g*fs.inodesPerGroup + idx + 1, nil
}

// allocBlock reserves one data block in group g and returns its absolute number.
func (fs *FS) allocBlock(g uint32, d []byte) (int64, error) {
	bmBlock := descBlockBitmap(d, fs.is64Bit)
	bm, err := fs.readBlock(bmBlock)
	if err != nil {
		return 0, fmt.Errorf("reading block bitmap for group %d: %w", g, err)
	}

	var idx uint32
	found := false
	for i := uint32(0); i < fs.blocksPerGroup; i++ {
		if !bitSet(bm, i) {
			idx, found = i, true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("group %d reported free blocks but the bitmap is full", g)
	}

	setBit(bm, idx)
	bmSize := int((fs.blocksPerGroup + 7) / 8)
	fs.bitmapCsumSet(d, bm, bmSize, 0x18, 0x38)
	if err := fs.writeBlock(bmBlock, bm); err != nil {
		return 0, err
	}

	setDescFreeBlocks(d, fs.is64Bit, descFreeBlocks(d, fs.is64Bit)-1)
	put16(d[0x12:], le16(d[0x12:])&^bgBlockUninit)

	// s_free_blocks_count, split across lo/hi when 64bit
	free := uint64(le32(fs.sb[0x0C:]))
	if fs.is64Bit {
		free |= uint64(le32(fs.sb[0x158:])) << 32
	}
	free--
	put32(fs.sb[0x0C:], uint32(free))
	if fs.is64Bit {
		put32(fs.sb[0x158:], uint32(free>>32))
	}

	return int64(fs.firstDataBlock) + int64(g)*int64(fs.blocksPerGroup) + int64(idx), nil
}

func (fs *FS) flushSuperblock() error {
	if fs.metadataCsum {
		put32(fs.sb[0x3FC:], 0)
		put32(fs.sb[0x3FC:], crc32c(^uint32(0), fs.sb[:0x3FC]))
	}
	return fs.writeAt(fs.sb, superblockOffset)
}

// ------------------------------------------------------------------- dirents

func direntLen(nameLen int) int { return (8 + nameLen + 3) &^ 3 }

// setDirBlockCsum fills in the checksum tail at the end of a directory block.
func (fs *FS) setDirBlockCsum(block []byte, dirIno uint32, gen []byte) {
	if !fs.metadataCsum {
		return
	}
	tail := block[len(block)-direntTailLen:]
	for i := range tail {
		tail[i] = 0
	}
	put32(tail[0:], 0)
	put16(tail[4:], direntTailLen)
	tail[6] = 0
	tail[7] = direntTailMark

	var num [4]byte
	put32(num[:], dirIno)

	crc := crc32c(fs.csumSeed, num[:])
	crc = crc32c(crc, gen)
	crc = crc32c(crc, block[:len(block)-direntTailLen])
	put32(tail[8:], crc)
}

// newDirBlock builds the "." and ".." block for a freshly created directory.
func (fs *FS) newDirBlock(self, parent uint32, gen []byte) []byte {
	b := make([]byte, fs.blockSize)
	limit := int(fs.blockSize)
	if fs.metadataCsum {
		limit -= direntTailLen
	}

	// "."
	put32(b[0:], self)
	put16(b[4:], 12)
	b[6] = 1
	b[7] = fileTypeDir
	copy(b[8:], ".")

	// ".." runs to the end of the usable area
	put32(b[12:], parent)
	put16(b[16:], uint16(limit-12))
	b[18] = 2
	b[19] = fileTypeDir
	copy(b[20:], "..")

	fs.setDirBlockCsum(b, self, gen)
	return b
}

// addDirent inserts an entry into a directory by splitting the slack at the
// end of the last entry in the block.
func (fs *FS) addDirent(dir *inode, name string, child uint32, fileType uint8) error {
	if dir.flags()&inodeFlagIndex != 0 {
		return fmt.Errorf("directory inode %d is a hashed (htree) directory; "+
			"this writer cannot insert into one safely", dir.num)
	}
	if dir.size() > uint64(fs.blockSize) {
		return fmt.Errorf("directory inode %d spans %d bytes (more than one block); "+
			"this writer only handles single-block directories", dir.num, dir.size())
	}

	blk, err := dir.firstExtentBlock()
	if err != nil {
		return err
	}
	b, err := fs.readBlock(blk)
	if err != nil {
		return err
	}

	limit := int(fs.blockSize)
	if fs.metadataCsum {
		limit -= direntTailLen
	}
	need := direntLen(len(name))

	// Walk to the last entry, checking for a name clash on the way.
	off := 0
	for off < limit {
		recLen := int(le16(b[off+4:]))
		if recLen < 8 || off+recLen > limit {
			return fmt.Errorf("directory inode %d has a malformed entry at offset %d", dir.num, off)
		}
		nameLen := int(b[off+6])
		if le32(b[off:]) != 0 && string(b[off+8:off+8+nameLen]) == name {
			return fmt.Errorf("%q already exists in directory inode %d", name, dir.num)
		}

		if off+recLen >= limit {
			// Last entry. Shrink it to its minimum and take the slack.
			used := direntLen(nameLen)
			if le32(b[off:]) == 0 {
				used = 0 // an unused entry can be overwritten entirely
			}
			if recLen-used < need {
				return fmt.Errorf("directory inode %d has no room for %q "+
					"(needs %d bytes, %d available); growing directories is not supported",
					dir.num, name, need, recLen-used)
			}
			if used == 0 {
				put32(b[off:], child)
				put16(b[off+4:], uint16(recLen))
				b[off+6] = byte(len(name))
				b[off+7] = fileType
				copy(b[off+8:], name)
			} else {
				put16(b[off+4:], uint16(used))
				n := off + used
				put32(b[n:], child)
				put16(b[n+4:], uint16(recLen-used))
				b[n+6] = byte(len(name))
				b[n+7] = fileType
				copy(b[n+8:], name)
			}
			fs.setDirBlockCsum(b, dir.num, dir.raw[0x64:0x68])
			return fs.writeBlock(blk, b)
		}
		off += recLen
	}
	return fmt.Errorf("directory inode %d: walked past the end of the block without finding a last entry", dir.num)
}

// ---------------------------------------------------------------- operations

func (fs *FS) lookup(dir *inode, name string) (uint32, error) {
	if dir.flags()&inodeFlagIndex != 0 {
		return 0, fmt.Errorf("directory inode %d is hashed (htree); lookup unsupported", dir.num)
	}
	blk, err := dir.firstExtentBlock()
	if err != nil {
		return 0, err
	}
	b, err := fs.readBlock(blk)
	if err != nil {
		return 0, err
	}
	limit := int(fs.blockSize)
	if fs.metadataCsum {
		limit -= direntTailLen
	}
	for off := 0; off < limit; {
		recLen := int(le16(b[off+4:]))
		if recLen < 8 {
			break
		}
		ino := le32(b[off:])
		nameLen := int(b[off+6])
		if ino != 0 && off+8+nameLen <= limit && string(b[off+8:off+8+nameLen]) == name {
			return ino, nil
		}
		off += recLen
	}
	return 0, nil // not found
}

func (fs *FS) initInode(in *inode, mode uint16, links uint16, size uint64, blk int64) {
	for i := range in.raw {
		in.raw[i] = 0
	}
	now := uint32(time.Now().Unix())
	put16(in.raw[0x00:], mode)
	put16(in.raw[0x02:], 0) // uid root
	put16(in.raw[0x18:], 0) // gid root
	put16(in.raw[0x1A:], links)
	put32(in.raw[0x08:], now) // atime
	put32(in.raw[0x0C:], now) // ctime
	put32(in.raw[0x10:], now) // mtime
	put32(in.raw[0x20:], inodeFlagExtents)
	put32(in.raw[0x1C:], uint32(fs.blockSize/512)) // i_blocks, 512-byte units
	if fs.inodeSize > 128 {
		put16(in.raw[0x80:], uint16(fs.inodeSize-128)) // i_extra_isize
	}
	in.setSize(size)
	in.setSingleExtent(blk)
}

// Mkdir creates a directory named name inside parent.
func (fs *FS) Mkdir(parent *inode, name string) (*inode, error) {
	// Check for a clash before allocating anything. Discovering it later, in
	// addDirent, would leave an allocated inode with nothing pointing at it —
	// e2fsck reports that as "Unattached inode".
	if existing, err := fs.lookup(parent, name); err != nil {
		return nil, err
	} else if existing != 0 {
		return nil, &ExistsError{Name: name, Inode: existing}
	}

	g, d, err := fs.pickGroup(true, true)
	if err != nil {
		return nil, err
	}
	ino, err := fs.allocInode(g, d, true)
	if err != nil {
		return nil, err
	}
	blk, err := fs.allocBlock(g, d)
	if err != nil {
		return nil, err
	}
	if err := fs.writeDesc(g, d); err != nil {
		return nil, err
	}

	in := &inode{num: ino, raw: make([]byte, fs.inodeSize)}
	fs.initInode(in, modeDir|0o755, 2, uint64(fs.blockSize), blk)

	if err := fs.writeBlock(blk, fs.newDirBlock(ino, parent.num, in.raw[0x64:0x68])); err != nil {
		return nil, err
	}
	if err := fs.writeInode(in); err != nil {
		return nil, err
	}
	if err := fs.addDirent(parent, name, ino, fileTypeDir); err != nil {
		return nil, err
	}

	// The new directory's ".." is an extra link to the parent.
	parent.setLinks(parent.links() + 1)
	if err := fs.writeInode(parent); err != nil {
		return nil, err
	}
	return in, nil
}

// CreateFile writes data as a new regular file inside parent. data must fit in
// a single block.
func (fs *FS) CreateFile(parent *inode, name string, data []byte) (*inode, error) {
	if int64(len(data)) > fs.blockSize {
		return nil, fmt.Errorf("%q is %d bytes; this writer emits single-block files only (max %d)",
			name, len(data), fs.blockSize)
	}

	if existing, err := fs.lookup(parent, name); err != nil {
		return nil, err
	} else if existing != 0 {
		return nil, &ExistsError{Name: name, Inode: existing}
	}

	g, d, err := fs.pickGroup(true, true)
	if err != nil {
		return nil, err
	}
	ino, err := fs.allocInode(g, d, false)
	if err != nil {
		return nil, err
	}
	blk, err := fs.allocBlock(g, d)
	if err != nil {
		return nil, err
	}
	if err := fs.writeDesc(g, d); err != nil {
		return nil, err
	}

	buf := make([]byte, fs.blockSize)
	copy(buf, data)
	if err := fs.writeBlock(blk, buf); err != nil {
		return nil, err
	}

	in := &inode{num: ino, raw: make([]byte, fs.inodeSize)}
	fs.initInode(in, modeFile|0o644, 1, uint64(len(data)), blk)
	if err := fs.writeInode(in); err != nil {
		return nil, err
	}
	if err := fs.addDirent(parent, name, ino, fileTypeRegular); err != nil {
		return nil, err
	}
	return in, nil
}

// ExistsError reports that a name is already present in the target directory.
// It is a distinct type so callers can offer to overwrite rather than failing.
type ExistsError struct {
	Name  string
	Inode uint32
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("%q already exists (inode %d)", e.Name, e.Inode)
}

// Overwrite replaces the contents of an existing single-block regular file.
//
// This allocates nothing: it rewrites the data block already mapped by the
// inode and updates the size. That makes re-injecting a different token onto
// the same base image the cheapest and safest operation in this file — there
// are no bitmaps or free counters to get wrong.
func (fs *FS) Overwrite(in *inode, data []byte) error {
	if in.isDir() {
		return fmt.Errorf("inode %d is a directory", in.num)
	}
	if int64(len(data)) > fs.blockSize {
		return fmt.Errorf("replacement is %d bytes; single-block files only (max %d)",
			len(data), fs.blockSize)
	}
	blk, err := in.firstExtentBlock()
	if err != nil {
		return err
	}

	buf := make([]byte, fs.blockSize)
	copy(buf, data)
	if err := fs.writeBlock(blk, buf); err != nil {
		return err
	}

	in.setSize(uint64(len(data)))
	now := uint32(time.Now().Unix())
	put32(in.raw[0x0C:], now) // ctime
	put32(in.raw[0x10:], now) // mtime
	return fs.writeInode(in)
}

// Resolve walks an absolute path and returns the inode, or nil if absent.
func (fs *FS) Resolve(parts []string) (*inode, error) {
	cur, err := fs.readInode(rootInode)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if p == "" {
			continue
		}
		if !cur.isDir() {
			return nil, fmt.Errorf("%q is not a directory", p)
		}
		ino, err := fs.lookup(cur, p)
		if err != nil {
			return nil, err
		}
		if ino == 0 {
			return nil, nil
		}
		if cur, err = fs.readInode(ino); err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// Sync flushes the superblock and the underlying file.
func (fs *FS) Sync() error {
	if err := fs.flushSuperblock(); err != nil {
		return err
	}
	return fs.f.Sync()
}

var _ = io.EOF
