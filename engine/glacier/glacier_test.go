package glacier

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DIYTechnologist/savesync-engine/engine"
	"github.com/DIYTechnologist/savesync-engine/gameapi"
)

// Arbitrary account, not a real player's.
const testAccount uint64 = 12345678

func testConfig() Config {
	return Config{Slots: []SlotConfig{{Logical: "slot0", Label: "Slot 1", SaveName: "sdimg_KntSlotSaveFile-0", PCDir: "kntslotsavefile-0"}}}
}

func testImages(steamID uint64) []gameapi.SaveImage {
	images := (Engine{}).Images(testConfig())
	for i := range images {
		images[i].SteamID = steamID
	}
	return images
}

// synthetic plaintext files, shaped like the real ones.
func synthetic(t *testing.T) (index, data []byte) {
	t.Helper()
	index = append(append([]byte{}, indexMagic...), bytes.Repeat([]byte{0x5a}, 300)...)
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write([]byte(strings.Repeat("ZDynamicObject Spawnpoint ", 50)))
	w.Close()
	return index, buf.Bytes()
}

func TestParseConfigRejectsMissingFields(t *testing.T) {
	for _, c := range []string{
		`{"slots":[]}`,
		`{"slots":[{"save_name":"s","pc_dir":"p"}]}`,
		`{"slots":[{"logical":"l","pc_dir":"p"}]}`,
		`{"slots":[{"logical":"l","save_name":"s"}]}`,
	} {
		if _, err := (Engine{}).ParseConfig(json.RawMessage(c)); err == nil {
			t.Errorf("expected an error for config %s", c)
		}
	}
}

func TestImagesTwoFilesPerSlot(t *testing.T) {
	images := (Engine{}).Images(testConfig())
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
	if images[0].Logical != "slot0-index" || images[0].Payload != IndexFile || images[0].PCFile != "kntslotsavefile-0/index.save" {
		t.Fatalf("unexpected index image: %+v", images[0])
	}
	if images[1].Logical != "slot0-data" || images[1].Payload != DataFile || images[1].SaveName != images[0].SaveName {
		t.Fatalf("unexpected data image: %+v", images[1])
	}
}

func TestKeyAcceptsAccountIDOrSteamID64(t *testing.T) {
	a, err := Key(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Key(steamID64Base | testAccount)
	if a != b {
		t.Fatalf("account ID and SteamID64 gave different keys: %x vs %x", a, b)
	}
	if a[4] != 0x01 || a[5] != 0x00 || a[6] != 0x10 || a[7] != 0x01 {
		t.Fatalf("key upper half should be the SteamID64 prefix, got %x", a)
	}
	if _, err := Key(0); err == nil {
		t.Fatal("expected an error without a steam id")
	}
}

func TestRoundTrip(t *testing.T) {
	index, data := synthetic(t)
	e := Engine{}
	images := testImages(testAccount)
	ps5 := map[string][]byte{"slot0-index": index, "slot0-data": data}

	toPC, err := e.ConvertFromPS5(testConfig(), images, ps5, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	pcIndex := toPC.Outputs["kntslotsavefile-0/index.save"]
	if bytes.Equal(pcIndex, index) {
		t.Fatal("PC output was not encrypted")
	}
	if owner, ok := KeyFromIndex(pcIndex); !ok || owner != steamID64Base|testAccount {
		t.Fatalf("KeyFromIndex = %d, %v", owner, ok)
	}
	if v := e.Inspect(testConfig(), "slot0-index", pcIndex, engine.SidePC, nil); !v.Portable {
		t.Fatalf("PC index failed inspect: %+v", v)
	}

	dir := t.TempDir()
	if err := e.InstallOutputs(testConfig(), toPC.Outputs, dir, filepath.Join(dir, "backup")); err != nil {
		t.Fatal(err)
	}
	toPS5, err := e.ConvertToPS5(testConfig(), images, dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(toPS5.Outputs["sdimg_KntSlotSaveFile-0/index.save"], index) || !bytes.Equal(toPS5.Outputs["sdimg_KntSlotSaveFile-0/data.save"], data) {
		t.Fatal("round trip did not reproduce the PS5 files")
	}
}

func TestConvertToPS5WrongAccountNamesOwner(t *testing.T) {
	index, data := synthetic(t)
	toPC, err := (Engine{}).ConvertFromPS5(testConfig(), testImages(testAccount), map[string][]byte{"slot0-index": index, "slot0-data": data}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for rel, b := range toPC.Outputs {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755)
		os.WriteFile(filepath.Join(dir, rel), b, 0o644)
	}
	_, err = (Engine{}).ConvertToPS5(testConfig(), testImages(testAccount+1), dir, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "76561197972611406") {
		t.Fatalf("expected an error naming the owning SteamID64, got %v", err)
	}
}

func TestInspectRejectsNonGlacier(t *testing.T) {
	junk := bytes.Repeat([]byte{0x42}, 64)
	for _, logical := range []string{"slot0-index", "slot0-data"} {
		if v := (Engine{}).Inspect(testConfig(), logical, junk, engine.SidePS5, nil); v.Portable || v.Tier != engine.TierWrongFormat {
			t.Errorf("%s: expected TierWrongFormat, got %+v", logical, v)
		}
	}
}
