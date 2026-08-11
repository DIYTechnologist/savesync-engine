// Package elbsave implements the outer container format of The Alters'
// save files - Capcom-unrelated, Unreal Engine 5.2 based, but not
// standard GVAS: 11 bit studios' own "ElbSaveMeta" save system. A world
// save (named "<random-id>-ACT<n>-day-<d>-v-<archive-version>.sav" on
// PC, "<random-id>v<archive-version>" as the PS5 Garlic save name) is:
//
//	[plaintext prefix: header, ElbSaveMeta, load-screen details, GUID lists]
//	[chunk]*                          <- until EOF
//
// Format facts (chunk framing, the two EOF-relative size fields and how
// to locate them structurally, the inner archive version marker) are
// cross-referenced against Iteratrix/alters-save's public Rust source
// for facts only, the same standard already applied to this project's
// RE Engine and Lime/Mandarin work - this Go implementation is
// original, not ported.
//
// No account-identity or platform marker was found anywhere in two real
// saves (one Steam, one PS5) - neither the prefix nor the decompressed
// body contains a Steam ID, PSN account ID, or platform enum of any
// kind, and the two samples' game-content class-path universes are
// effectively identical (1423 vs 1419 distinct paths, same names). That
// makes this the simplest conversion shape in the project so far: a
// save's bytes carry no platform-specific state to retarget at all.
package elbsave

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// chunkTagValue is the 8-byte little-endian tag every chunk header
// starts with. Its low 4 bytes (chunkMagic) alone are used to find a
// candidate chunk start by scanning; the full 8-byte tag then confirms
// it.
const chunkTagValue uint64 = 0x2222_2222_9E2A_83C1
const chunkMagic uint32 = 0x9E2A_83C1

const (
	chunkBlockSize  = 0x2_0000 // 128 KiB per chunk (the last one may be smaller)
	chunkHeaderLen  = 8 + 8 + 1 + 32
	minSizeField    = 1000 // EOF-relative size fields are always far bigger than this
	maxArchiveVer   = 64   // sanity bound for the inner archive-version int
	compressionZlib = 3
)

// ArchiveVersion is the inner archive's own serialization version,
// found immediately after the inner EOF-relative size field. Observed
// values 2 (older builds) and 3 (current) - the tagged-property header
// layout inside the body differs between them, but this package doesn't
// need to know that: it treats the body as an opaque byte stream.
type ArchiveVersion int32

// SaveFile is a parsed .sav file: the plaintext prefix plus the
// concatenated, decompressed body of every chunk.
type SaveFile struct {
	Prefix []byte
	// Body is the decompressed body - the concatenation of every
	// inflated chunk, a stream of the game's own object records. This
	// package never needs to interpret it: the format carries no
	// platform-specific state (see the package doc comment), so a
	// conversion only needs to move these bytes into the destination's
	// framing unchanged.
	Body []byte
	// eofRelativeFields are the offsets, within Prefix, of the two
	// little-endian int32 fields whose value equals
	// len(fullFile) - offset - 4 ([0]=inner archive size, [1]=total
	// compressed-payload size). Both must be rewritten by Build if the
	// compressed payload's length changes.
	eofRelativeFields [2]int
	ArchiveVer        ArchiveVersion
}

func readI32(b []byte, off int) (int32, bool) {
	if off < 0 || off+4 > len(b) {
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(b[off : off+4])), true
}

func readU64(b []byte, off int) (uint64, bool) {
	if off < 0 || off+8 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b[off : off+8]), true
}

type chunkHeader struct {
	format     byte
	compressed int
	uncompress int
}

func parseChunkHeader(b []byte, off int) (chunkHeader, error) {
	tag, ok := readU64(b, off)
	if !ok || tag != chunkTagValue {
		return chunkHeader{}, fmt.Errorf("chunk at %#x: bad or truncated tag", off)
	}
	block, ok := readU64(b, off+8)
	if !ok || block != chunkBlockSize {
		return chunkHeader{}, fmt.Errorf("chunk at %#x: unexpected block size", off)
	}
	if off+17 > len(b) {
		return chunkHeader{}, fmt.Errorf("chunk at %#x: truncated format byte", off)
	}
	format := b[off+16]
	sizes := make([]uint64, 4)
	for i := 0; i < 4; i++ {
		v, ok := readU64(b, off+17+8*i)
		if !ok {
			return chunkHeader{}, fmt.Errorf("chunk at %#x: truncated size table", off)
		}
		sizes[i] = v
	}
	totalComp, totalUncomp, comp, uncomp := sizes[0], sizes[1], sizes[2], sizes[3]
	if totalComp != comp || totalUncomp != uncomp {
		return chunkHeader{}, fmt.Errorf("chunk at %#x: multi-block chunk (unsupported)", off)
	}
	if uncomp > chunkBlockSize {
		return chunkHeader{}, fmt.Errorf("chunk at %#x: uncompressed size exceeds block size", off)
	}
	return chunkHeader{format: format, compressed: int(comp), uncompress: int(uncomp)}, nil
}

func findChunkStart(b []byte) int {
	needle := make([]byte, 4)
	binary.LittleEndian.PutUint32(needle, chunkMagic)
	for i := 0; i+4 <= len(b); {
		idx := bytes.Index(b[i:], needle)
		if idx < 0 {
			return -1
		}
		candidate := i + idx
		if _, err := parseChunkHeader(b, candidate); err == nil {
			return candidate
		}
		i = candidate + 1
	}
	return -1
}

func isEOFRelative(prefix []byte, offset, fileLen int) bool {
	v, ok := readI32(prefix, offset)
	if !ok {
		return false
	}
	value := int64(v)
	return value >= minSizeField && int64(offset)+4+value == int64(fileLen)
}

func hasInnerHeaderSignature(prefix []byte, offset int) bool {
	version, ok := readI32(prefix, offset+4)
	if !ok || version < 1 || version > maxArchiveVer {
		return false
	}
	if offset+12 > len(prefix) {
		return false
	}
	return bytes.Equal(prefix[offset+8:offset+12], []byte{1, 0, 0, 0})
}

// findEOFRelativeFields locates the two EOF-relative size fields
// structurally, exactly as the reference implementation does: the
// compressed-total field always occupies the last 4 bytes of the
// prefix; the inner-archive field is found by scanning the whole prefix
// for the one offset that is both EOF-relative and immediately followed
// by a small archive-version int and the fixed marker [1,0,0,0].
func findEOFRelativeFields(prefix []byte, fileLen int) ([2]int, error) {
	compressedTotal := len(prefix) - 4
	if compressedTotal < 0 || !isEOFRelative(prefix, compressedTotal, fileLen) {
		return [2]int{}, errors.New("elbsave: no valid compressed-total size field found")
	}
	var inner = -1
	count := 0
	for off := 0; off < len(prefix)-3; off++ {
		if isEOFRelative(prefix, off, fileLen) && hasInnerHeaderSignature(prefix, off) {
			inner = off
			count++
		}
	}
	if count != 1 {
		return [2]int{}, fmt.Errorf("elbsave: expected exactly one inner size field, found %d", count)
	}
	return [2]int{inner, compressedTotal}, nil
}

// Parse parses a raw .sav file.
func Parse(data []byte) (*SaveFile, error) {
	start := findChunkStart(data)
	if start < 0 {
		return nil, errors.New("elbsave: no chunk stream found - not an ElbSaveMeta world save")
	}
	prefix := append([]byte(nil), data[:start]...)
	fields, err := findEOFRelativeFields(prefix, len(data))
	if err != nil {
		return nil, err
	}
	archiveVer := ArchiveVersion(0)
	if v, ok := readI32(prefix, fields[0]+4); ok {
		archiveVer = ArchiveVersion(v)
	}

	var body []byte
	off := start
	for off < len(data) {
		hdr, err := parseChunkHeader(data, off)
		if err != nil {
			return nil, err
		}
		if hdr.format != compressionZlib {
			return nil, fmt.Errorf("elbsave: unsupported compression format %d", hdr.format)
		}
		dataStart := off + chunkHeaderLen
		dataEnd := dataStart + hdr.compressed
		if dataEnd > len(data) {
			return nil, fmt.Errorf("elbsave: chunk at %#x: compressed data runs past EOF", off)
		}
		r, err := zlib.NewReader(bytes.NewReader(data[dataStart:dataEnd]))
		if err != nil {
			return nil, fmt.Errorf("elbsave: chunk at %#x: %w", off, err)
		}
		block, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("elbsave: chunk at %#x: inflate: %w", off, err)
		}
		if len(block) != hdr.uncompress {
			return nil, fmt.Errorf("elbsave: chunk at %#x: inflated %d bytes, header declared %d", off, len(block), hdr.uncompress)
		}
		body = append(body, block...)
		off = dataEnd
	}

	return &SaveFile{
		Prefix:            prefix,
		Body:              body,
		eofRelativeFields: fields,
		ArchiveVer:        archiveVer,
	}, nil
}

// SizeFieldOffsets returns the two EOF-relative size field offsets
// within Prefix, for callers that want to compare prefixes while
// ignoring the fields Build always rewrites.
func (s *SaveFile) SizeFieldOffsets() [2]int { return s.eofRelativeFields }

// Build rebuilds a complete .sav file from s.Prefix and s.Body,
// recompressing the body into 128 KiB zlib chunks (best compression, to
// match what real saves use) and rewriting both EOF-relative size
// fields in the prefix so the result is internally consistent
// regardless of how the body's length changed.
func (s *SaveFile) Build() ([]byte, error) {
	var payload bytes.Buffer
	for start := 0; start < len(s.Body); start += chunkBlockSize {
		end := start + chunkBlockSize
		if end > len(s.Body) {
			end = len(s.Body)
		}
		block := s.Body[start:end]

		var compBuf bytes.Buffer
		w, err := zlib.NewWriterLevel(&compBuf, zlib.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(block); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}

		var hdr [chunkHeaderLen]byte
		binary.LittleEndian.PutUint64(hdr[0:8], chunkTagValue)
		binary.LittleEndian.PutUint64(hdr[8:16], chunkBlockSize)
		hdr[16] = compressionZlib
		compLen := uint64(compBuf.Len())
		uncompLen := uint64(len(block))
		binary.LittleEndian.PutUint64(hdr[17:25], compLen)
		binary.LittleEndian.PutUint64(hdr[25:33], uncompLen)
		binary.LittleEndian.PutUint64(hdr[33:41], compLen)
		binary.LittleEndian.PutUint64(hdr[41:49], uncompLen)
		payload.Write(hdr[:])
		payload.Write(compBuf.Bytes())
	}

	out := append([]byte(nil), s.Prefix...)
	out = append(out, payload.Bytes()...)
	for _, off := range s.eofRelativeFields {
		value := len(out) - off - 4
		if value < 0 || value > int(^uint32(0)>>1) {
			return nil, fmt.Errorf("elbsave: rebuilt size field at %#x out of i32 range: %d", off, value)
		}
		binary.LittleEndian.PutUint32(out[off:off+4], uint32(value))
	}
	return out, nil
}
