package elbsave

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math/rand"
	"testing"
)

// pseudoRandomBytes returns deterministic, poorly-compressible filler -
// real save bodies compress to well over the format's 1000-byte
// minimum-size-field threshold, but the repetitive filler a naive test
// body would use compresses away to almost nothing, tripping that
// threshold for reasons that have nothing to do with what a test is
// actually exercising.
func pseudoRandomBytes(n int) []byte {
	r := rand.New(rand.NewSource(1))
	b := make([]byte, n)
	r.Read(b)
	return b
}

// buildTestFile assembles a minimal synthetic .sav file: a plaintext
// prefix carrying the two EOF-relative size fields at realistic
// positions (an "inner" one near the front, "total" as the prefix's
// last 4 bytes) plus a real chunk stream, so these tests never depend
// on committing real save data.
func buildTestFile(t *testing.T, body []byte, archiveVersion int32) []byte {
	t.Helper()
	return buildTestFileWithPayloadPrefix(t, nil, body, archiveVersion)
}

// buildTestFileWithPayloadPrefix is buildTestFile, but with junk bytes
// inserted right before the real chunk stream - still accounted for in
// the size fields, unlike post-hoc splicing onto an already-built file
// (which would leave the size fields stale by the spliced length).
func buildTestFileWithPayloadPrefix(t *testing.T, payloadPrefix, body []byte, archiveVersion int32) []byte {
	t.Helper()

	// Everything before the inner size field (arbitrary filler, standing
	// in for the file's header/ElbSaveMeta/GUID-list bytes this package
	// never interprets).
	before := bytes.Repeat([]byte{0xAA}, 20)

	// Everything between the inner field's signature and the prefix's
	// end (stands in for the rest of the real prefix's content). Sized
	// generously so the EOF-relative size fields' values clear
	// minSizeField even for a test file with a tiny body.
	between := bytes.Repeat([]byte{0xBB}, 1200)

	// Assemble the chunk stream first, since the size fields' values
	// depend on the total file length.
	var payload bytes.Buffer
	payload.Write(payloadPrefix)
	for start := 0; start < len(body); start += chunkBlockSize {
		end := start + chunkBlockSize
		if end > len(body) {
			end = len(body)
		}
		block := body[start:end]
		var comp bytes.Buffer
		w := zlib.NewWriter(&comp)
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		var hdr [chunkHeaderLen]byte
		binary.LittleEndian.PutUint64(hdr[0:8], chunkTagValue)
		binary.LittleEndian.PutUint64(hdr[8:16], chunkBlockSize)
		hdr[16] = compressionZlib
		compLen := uint64(comp.Len())
		uncompLen := uint64(len(block))
		binary.LittleEndian.PutUint64(hdr[17:25], compLen)
		binary.LittleEndian.PutUint64(hdr[25:33], uncompLen)
		binary.LittleEndian.PutUint64(hdr[33:41], compLen)
		binary.LittleEndian.PutUint64(hdr[41:49], uncompLen)
		payload.Write(hdr[:])
		payload.Write(comp.Bytes())
	}

	// inner field placement: [i32 value][i32 archiveVersion][01 00 00 00][between...]
	innerBlock := make([]byte, 12+len(between))
	binary.LittleEndian.PutUint32(innerBlock[4:8], uint32(archiveVersion))
	copy(innerBlock[8:12], []byte{1, 0, 0, 0})
	copy(innerBlock[12:], between)

	prefixLen := len(before) + len(innerBlock) + 4 // +4 for the trailing total field
	innerOffset := len(before)
	totalOffset := prefixLen - 4

	fileLen := prefixLen + payload.Len()
	innerValue := fileLen - innerOffset - 4
	totalValue := fileLen - totalOffset - 4
	binary.LittleEndian.PutUint32(innerBlock[0:4], uint32(innerValue))

	var out bytes.Buffer
	out.Write(before)
	out.Write(innerBlock)
	var totalField [4]byte
	binary.LittleEndian.PutUint32(totalField[:], uint32(totalValue))
	out.Write(totalField[:])
	out.Write(payload.Bytes())
	return out.Bytes()
}

func TestParseRoundTrip(t *testing.T) {
	body := pseudoRandomBytes(220000) // spans multiple 128KiB chunks, poorly-compressible
	data := buildTestFile(t, body, 3)

	sf, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if sf.ArchiveVer != 3 {
		t.Errorf("ArchiveVer = %d, want 3", sf.ArchiveVer)
	}
	if !bytes.Equal(sf.Body, body) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(sf.Body), len(body))
	}

	rebuilt, err := sf.Build()
	if err != nil {
		t.Fatal(err)
	}
	sf2, err := Parse(rebuilt)
	if err != nil {
		t.Fatalf("reparse of rebuilt file: %v", err)
	}
	if !bytes.Equal(sf2.Body, body) {
		t.Fatal("body changed across a rebuild round trip")
	}
	if sf2.SizeFieldOffsets() != sf.SizeFieldOffsets() {
		t.Fatal("size field offsets moved across a rebuild round trip")
	}
}

func TestParseSingleSmallChunk(t *testing.T) {
	body := pseudoRandomBytes(2000) // small, but still clears the 1000-byte size-field threshold once compressed
	data := buildTestFile(t, body, 2)
	sf, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sf.Body, body) {
		t.Fatalf("body = %q, want %q", sf.Body, body)
	}
	if sf.ArchiveVer != 2 {
		t.Errorf("ArchiveVer = %d, want 2", sf.ArchiveVer)
	}
}

func TestBuildChangesLengthCorrectly(t *testing.T) {
	data := buildTestFile(t, pseudoRandomBytes(2000), 3)
	sf, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	// Grow the body enough to change the compressed payload's length, and
	// confirm the rebuilt file's size fields are updated consistently
	// rather than left stale.
	sf.Body = append(sf.Body, pseudoRandomBytes(5000)...)
	rebuilt, err := sf.Build()
	if err != nil {
		t.Fatal(err)
	}
	sf2, err := Parse(rebuilt)
	if err != nil {
		t.Fatalf("grown file failed to reparse: %v", err)
	}
	if !bytes.Equal(sf2.Body, sf.Body) {
		t.Fatal("grown body did not survive the round trip")
	}
}

func TestParseRejectsFileWithNoChunkStream(t *testing.T) {
	if _, err := Parse(bytes.Repeat([]byte{0x00}, 100)); err == nil {
		t.Fatal("expected an error for a file with no chunk tag anywhere")
	}
}

func TestParseRejectsTruncatedChunk(t *testing.T) {
	data := buildTestFile(t, pseudoRandomBytes(5000), 3)
	// Truncate partway through the chunk stream.
	truncated := data[:len(data)-50]
	if _, err := Parse(truncated); err == nil {
		t.Fatal("expected an error for a truncated chunk")
	}
}

func TestParseRejectsBadCompressionFormat(t *testing.T) {
	data := buildTestFile(t, pseudoRandomBytes(2000), 3)
	start := findChunkStart(data)
	if start < 0 {
		t.Fatal("test file has no chunk start")
	}
	corrupted := append([]byte(nil), data...)
	corrupted[start+16] = 0x99 // format byte: not zlib(3)
	if _, err := Parse(corrupted); err == nil {
		t.Fatal("expected an error for an unsupported compression format")
	}
}

func TestFindChunkStartSkipsCoincidentalMagicBytes(t *testing.T) {
	body := pseudoRandomBytes(2000)
	data := buildTestFile(t, body, 3)

	// Plant a bare copy of the magic (not a valid full chunk header)
	// inside the prefix's filler region, well before the real chunk
	// stream - overwriting filler bytes in place keeps every size field
	// valid, since neither the file length nor the prefix length changes.
	// The scan must skip past this decoy rather than mistaking it for the
	// real start.
	decoyAt := 40 // inside the "between" filler, away from the tracked fields
	binary.LittleEndian.PutUint32(data[decoyAt:decoyAt+4], chunkMagic)

	sf, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sf.Body, body) {
		t.Fatal("parser was fooled by a coincidental magic-byte match")
	}
}
