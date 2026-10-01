// Package glacier converts saves for IO Interactive's Glacier engine,
// confirmed against 007 First Light. See docs/007firstlight.md.
//
// Every save slot is a directory (PC) or Garlic image (PS5) holding two
// files: index.save, a typed header ("SSaveGameHeader"), and data.save,
// the zlib-compressed game state. Both platforms store the same bytes;
// the Steam side just XORs each file with the player's SteamID64,
// little-endian, repeated every 8 bytes from the start of the file. The
// PS5 side is plaintext. Conversion is that XOR and nothing else - the
// header's hash, size, timestamp and version fields pass through
// untouched.
package glacier

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/DIYTechnologist/savesync-engine/engine"
	"github.com/DIYTechnologist/savesync-engine/gameapi"
	"github.com/DIYTechnologist/savesync-engine/util"
)

// SlotConfig describes one save slot: a Garlic image on PS5 and the
// matching directory under Steam's remote/ folder on PC. Each slot
// expands into two gameapi.SaveImages, one per file (see Images).
type SlotConfig struct {
	Logical string `json:"logical"`
	Label   string `json:"label"`
	// SaveName is the Garlic save image name, e.g. "sdimg_KntSlotSaveFile-0".
	SaveName string `json:"save_name"`
	// PCDir is the slot's directory under the PC save root, e.g.
	// "kntslotsavefile-0" (Steam Cloud stores these lowercased).
	PCDir string `json:"pc_dir"`
}

// Config is the engine_config block for a games/<key>.json profile using
// the "glacier" engine.
type Config struct {
	Slots []SlotConfig `json:"slots"`
}

const (
	IndexFile = "index.save"
	DataFile  = "data.save"
)

// CheckContainer is the only check: a payload that doesn't decode is
// either not a Glacier save or (PC side) was encrypted for a different
// Steam account. Neither is overridable.
const CheckContainer = "container"

// steamID64Base is the fixed upper half of an individual account's
// SteamID64. bridge.go masks --steam-id to the 32-bit account ID, so the
// key is rebuilt from it here.
const steamID64Base uint64 = 0x0110000100000000

// indexMagic is the start of every index.save seen so far, plaintext:
// a version/count prefix, then the length-prefixed root type name.
var indexMagic = append([]byte{0x03, 0x00, 0x00, 0x00, 0x01, 0x0f, 0x00, 0x00, 0x80}, "SSaveGameHeader"...)

type Engine struct{}

func New() Engine { return Engine{} }

func (Engine) Name() string { return "glacier" }

func (Engine) OverrideTokens() []string { return nil }

func (Engine) ParseConfig(raw json.RawMessage) (any, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid glacier engine_config: %w", err)
	}
	if len(cfg.Slots) == 0 {
		return nil, fmt.Errorf("glacier engine_config has no slots")
	}
	for _, s := range cfg.Slots {
		if s.Logical == "" || s.SaveName == "" || s.PCDir == "" {
			return nil, fmt.Errorf("glacier engine_config slot entries require logical, save_name, and pc_dir")
		}
	}
	return cfg, nil
}

// Images lists two images per slot, sharing a SaveName: "<logical>-index"
// and "<logical>-data". PCFile is "<pc_dir>/<file>".
func (Engine) Images(cfgAny any) []gameapi.SaveImage {
	cfg := cfgAny.(Config)
	var out []gameapi.SaveImage
	for _, s := range cfg.Slots {
		for _, file := range []string{IndexFile, DataFile} {
			out = append(out, gameapi.SaveImage{
				Logical:  s.Logical + "-" + strings.TrimSuffix(file, ".save"),
				SaveName: s.SaveName,
				Label:    s.Label,
				PCFile:   s.PCDir + "/" + file,
				Payload:  file,
			})
		}
	}
	return out
}

func (Engine) ResolvePayload(_ any, image gameapi.SaveImage, _ []string) (string, error) {
	return image.Payload, nil
}

func (Engine) ResolvePCFile(_ any, image gameapi.SaveImage, _ string) (string, error) {
	return image.PCFile, nil
}

func (Engine) Compatibility(any) gameapi.Compatibility {
	return gameapi.Compatibility{
		PC:          gameapi.CompatibilitySide{Platform: "Steam"},
		PS5:         gameapi.CompatibilitySide{Platform: "PS5"},
		Convertible: true,
		Note:        "Same save bytes on both platforms; Steam XORs each file with the account's SteamID64 (needs --steam-id). PC->PS5 confirmed loading in-game; PS5->PC not yet tested in-game. See docs/007firstlight.md.",
	}
}

// Key returns the 8-byte XOR key for a Steam account. accountID may be the
// 32-bit account ID or a full SteamID64.
func Key(accountID uint64) ([8]byte, error) {
	var key [8]byte
	if accountID == 0 {
		return key, errors.New("Glacier Steam saves are encrypted with the account's SteamID64 - pass --steam-id")
	}
	if accountID>>32 == 0 {
		accountID |= steamID64Base
	}
	binary.LittleEndian.PutUint64(key[:], accountID)
	return key, nil
}

// XOR applies (or removes - it's symmetric) the Steam-side obfuscation.
func XOR(data []byte, key [8]byte) []byte {
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = b ^ key[i%8]
	}
	return out
}

// KeyFromIndex recovers the XOR key a PC index.save was written with, from
// its known plaintext prefix. Used to tell the user which account a save
// belongs to when --steam-id doesn't match.
func KeyFromIndex(pcIndex []byte) (uint64, bool) {
	if len(pcIndex) < len(indexMagic) {
		return 0, false
	}
	var key [8]byte
	for i := range key {
		key[i] = pcIndex[i] ^ indexMagic[i]
	}
	if !bytes.Equal(XOR(pcIndex[:len(indexMagic)], key), indexMagic) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(key[:]), true
}

// validate checks a plaintext payload is the file it claims to be.
func validate(payload []byte, file string) error {
	if file == IndexFile {
		if !bytes.HasPrefix(payload, indexMagic) {
			return errors.New("index.save does not start with an SSaveGameHeader")
		}
		return nil
	}
	r, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("data.save is not zlib: %w", err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("data.save does not decompress: %w", err)
	}
	return nil
}

func containerCheck(logical string, err error) engine.Verdict {
	check := engine.CheckResult{Logical: logical, Check: CheckContainer, Tier: engine.TierWrongFormat, Passed: err == nil}
	if err != nil {
		check.Reason = err.Error()
		return engine.Verdict{Tier: engine.TierWrongFormat, Checks: []engine.CheckResult{check}}
	}
	return engine.Verdict{Portable: true, Checks: []engine.CheckResult{check}}
}

// Inspect validates one payload. A PS5 payload is plaintext. A PC
// index.save is checked by recovering its key from the known header;
// a PC data.save can't be checked without its key, so it's only
// validated during conversion (see decodePC).
func (Engine) Inspect(_ any, logical string, payload []byte, side engine.Side, _ map[string]bool) engine.Verdict {
	file := DataFile
	if strings.HasSuffix(logical, "-index") {
		file = IndexFile
	}
	if side == engine.SidePS5 {
		return containerCheck(logical, validate(payload, file))
	}
	if file == IndexFile {
		if _, ok := KeyFromIndex(payload); !ok {
			return containerCheck(logical, errors.New("index.save is not a Steam-encrypted SSaveGameHeader"))
		}
	}
	return containerCheck(logical, nil)
}

// ConvertFromPS5 encrypts each PS5 file for the --steam-id account.
// Output keys are the images' PCFile paths ("<pc_dir>/<file>").
func (Engine) ConvertFromPS5(_ any, images []gameapi.SaveImage, ps5Payloads map[string][]byte, pcDir string, _ map[string]bool) (gameapi.ConversionResult, error) {
	outputs := map[string][]byte{}
	manifest := map[string]any{"pc_dir": pcDir}
	for _, img := range images {
		data, ok := ps5Payloads[img.Logical]
		if !ok {
			return gameapi.ConversionResult{}, fmt.Errorf("missing PS5 payload for %s", img.Logical)
		}
		if err := validate(data, img.Payload); err != nil {
			return gameapi.ConversionResult{}, fmt.Errorf("%s: %w", img.Logical, err)
		}
		key, err := Key(img.SteamID)
		if err != nil {
			return gameapi.ConversionResult{}, err
		}
		outputs[img.PCFile] = XOR(data, key)
		manifest[img.Logical] = map[string]any{"ps5_save": img.SaveName, "pc_file": img.PCFile}
	}
	return gameapi.ConversionResult{Outputs: outputs, Manifest: manifest}, nil
}

// ConvertToPS5 decrypts each PC file. Output keys are "<SaveName>/<Payload>"
// since every slot's image carries two payloads.
func (Engine) ConvertToPS5(_ any, images []gameapi.SaveImage, pcDir string, _ map[string][]byte, _ map[string]bool) (gameapi.ConversionResult, error) {
	outputs := map[string][]byte{}
	manifest := map[string]any{"pc_dir": pcDir}
	for _, img := range images {
		raw, err := os.ReadFile(filepath.Join(pcDir, img.PCFile))
		if err != nil {
			return gameapi.ConversionResult{}, fmt.Errorf("%s: %w", img.Logical, err)
		}
		plain, err := decodePC(raw, img)
		if err != nil {
			return gameapi.ConversionResult{}, fmt.Errorf("%s: %w", img.Logical, err)
		}
		outputs[img.SaveName+"/"+img.Payload] = plain
		manifest[img.Logical] = map[string]any{"pc_file": img.PCFile, "ps5_save": img.SaveName, "payload_name": img.Payload}
	}
	return gameapi.ConversionResult{Outputs: outputs, Manifest: manifest}, nil
}

func decodePC(raw []byte, img gameapi.SaveImage) ([]byte, error) {
	key, err := Key(img.SteamID)
	if err != nil {
		return nil, err
	}
	plain := XOR(raw, key)
	if err := validate(plain, img.Payload); err != nil {
		if img.Payload == IndexFile {
			if owner, ok := KeyFromIndex(raw); ok {
				return nil, fmt.Errorf("this save belongs to SteamID64 %d, not the --steam-id given", owner)
			}
		}
		return nil, fmt.Errorf("does not decode with the --steam-id key (wrong account?): %w", err)
	}
	return plain, nil
}

// InstallOutputs writes each "<pc_dir>/<file>" output under pcDir, backing
// up any file it replaces first.
func (Engine) InstallOutputs(_ any, outputs map[string][]byte, pcDir string, backupDir string) error {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return err
	}
	for rel, data := range outputs {
		dest := filepath.Join(pcDir, rel)
		backup := filepath.Join(backupDir, rel)
		if _, err := os.Stat(dest); err == nil {
			if _, err := os.Stat(backup); os.IsNotExist(err) {
				if err := util.CopyFile(dest, backup); err != nil {
					return err
				}
			}
		}
		if err := util.AtomicWrite(dest, data); err != nil {
			return err
		}
	}
	return nil
}
