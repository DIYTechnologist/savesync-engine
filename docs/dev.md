# Engine Architecture

This module (`github.com/DIYTechnologist/savesync-engine`) is the save-format conversion library: container/cipher decode-encode, RSZ/GVAS/LSPK/TLV field handling, and the `engine.Engine` registry that ties a `games/<key>.json` profile to the code that reads/converts/writes its save format. It has no CLI or UI of its own - `savesyncpspc` (the `save-sync`/`save-sync-ui` binaries) is the primary consumer, importing this module and supplying its own `--garlic`/backup/apply orchestration around it. See that repo's `docs/dev.md` for CLI usage, build/release/CI, and product-level workflow; this document covers the library's own internal architecture. See [`docs/supported_games.md`](supported_games.md) for the per-game catalog (title IDs, engine, container/cipher facts, confirmed status).

## Engine Abstraction

Conversion logic is a property of the save-game *engine* (e.g. Unreal's GVAS format), not of any one title. A "game" is just a `games/<key>.json` profile naming an engine and supplying that engine's config; there is no per-game Go plugin to write.

```
games/<key>.json ──► profile loader (games) ──► engine registry (engine)
                                                              │
                          ┌───────────────────┬───────────────┴──────────────┬───────────────────┐
                          ▼                   ▼                              ▼                    ▼
                  engine/unreal         engine/larian                engine/reengine        engine/unityblb
                  (GVAS - Clair)          (LSPK - BG3)            (RE Engine DSSS - RE2)   (gzip+TLV - Subnautica)
```

(diagram source: [`docs/diagrams/engine-architecture.md`](diagrams/engine-architecture.md), mermaid version for GitHub rendering)

- `engine`: the `Engine` interface (`Name`, `ParseConfig`, `OverrideTokens`, `Images`, `Compatibility`, `Inspect`, `ResolvePayload`, `ResolvePCFile`, `ConvertFromPS5`, `ConvertToPS5`, `InstallOutputs`) and a name → `Engine` registry (`Register`/`Get`). `Config` is `any`: each engine parses its own `engine_config` block and type-asserts it back internally, so different engines can have completely different config shapes. Also defines the portability-gate vocabulary shared across engines: `Tier` (`TierConvertible` / `TierBlocked` / `TierWrongFormat`), `Side` (`SidePC` / `SidePS5`), `CheckResult`, and `Verdict`.
- `engine/unreal`: the first fully-implemented engine (read + convert + write). `Config` carries the Unreal module name, the list of Garlic save images (each with its own `payload` filename — there's no single game-wide `PayloadName()`), and a `class_equivalence` table of known-good `(pc class, ps5 class)` pairs per logical image. Every field on its images is config-known/static, so `ResolvePayload`/`ResolvePCFile` are trivial passthroughs it never actually needs.
- `engine/larian`: the second fully-implemented engine (LSPK container parsing/rebuilding + `Inspect` + `ConvertToPS5`/`ConvertFromPS5`, see below). Unlike Clair, none of a BG3 image's filenames are config-known — see "Dynamic image resolution" below for how that's handled generically rather than as a one-off hack.
- `engine/reengine`: Capcom RE Engine's DSSS container (Resident Evil 2 and siblings), built on the same dynamic-slot model as BG3 (a save's slot number isn't config-known, so `--ps5-save-name`, supplied by the calling CLI, picks it) but with the PC-side filename *derived* from the resolved slot rather than discovered by glob, since a PC save directory holds every slot's file side by side. See "Resident Evil 2" below and `docs/ressave.md` for the full format writeup (DSSS/Blowfish-LE container, RSZ field re-alignment, platform-field retargeting), and `TODO.md` for the wider RE Engine family (RE3/RE4/RE7/Village/Requiem).
- `engine/unityblb`: Subnautica's gzip+flat-entry container - the simplest engine here (no encryption, no proprietary class/versioning system to patch). Every image field is config-known/static like Unreal's, except its `PCFile` names a whole directory tree rather than one file (the calling CLI's backup path needs to be directory-aware). See `docs/subnautica.md`.
- `gvas`: unchanged low-level GVAS binary parsing/envelope-graft library (`Parse`, `ConvertWithEnvelope`), used by `engine/unreal` rather than by a game package directly.
- `games/registry.go` wires it together: `Profiles(gamesDir, builtin)` loads `games/*.json` (a caller-supplied embedded default - this module's own `embed.go`'s `Builtin`, or a consumer's own - merged with on-disk overrides under `gamesDir`), and `ResolveEngine(profile)` looks up `profile.Engine` in the registry and calls its `ParseConfig(profile.EngineConfig)`. `SelectProfile(...)` returns a `Selected{Profile, Engine, Config}` bundle a consuming CLI calls directly — there's no per-game Go code in that path at all. `builtin` is injected (`io/fs.FS`) rather than hardcoded, so this package has no dependency on any particular consumer's embed setup.

### Dynamic image resolution (games with no fixed filenames)

Clair's `gameapi.SaveImage` fields (`SaveName`, `Payload`, `PCFile`) are all config-known constants from `games/clair.json`. Baldur's Gate 3 has none of that: the PS5 save slot is whichever of Garlic's `sdimg_Save0001`/`sdimg_Save0002`/... the caller picked in-game, the `.lsv` filename inside that slot is the save's own display name (`A Nautiloid in Hell - 0h 00m.lsv`, `Ruined Battlefield - 39h 05m.lsv`, ...), and the PC-side filename is the same story on Steam's save folder. None of that can live in a JSON profile.

`gameapi.SaveImage` has three `Dynamic*` booleans (`DynamicSaveName`, `DynamicPayload`, `DynamicPCFile`) an engine sets on an image (`games/bg3.json` sets all three) to mark which fields must be resolved at runtime instead of read from config. Resolution itself is generic, not BG3-specific, but the *mechanism* that drives it (mounting a Garlic slot, prompting for `--ps5-save-name`) lives in the consuming CLI (`savesyncpspc`'s `bridge.resolveDynamicImages`), not in this module:

- `DynamicSaveName` — filled from the caller's own save-name selection; there's no way to discover "which save slot did you mean" from Garlic alone, so this one requires the caller to say so explicitly.
- `DynamicPayload` — resolved by mounting the now-known save slot and calling the engine's `Engine.ResolvePayload(cfg, image, mountedFileNames)`. Larian's implementation finds the single `.lsv` among the mounted files (errors if zero or more than one).
- `DynamicPCFile` — resolved by calling `Engine.ResolvePCFile(cfg, image, pcDir)`, only for `pc-to-ps5` (the PC side is the conversion source there; for `ps5-to-pc` the PC file doesn't exist yet, it's an output the engine names itself). Larian's implementation finds the single `.lsv` in the PC save directory.
- `engine.Engine` grew `ResolvePayload`/`ResolvePCFile` methods to support this. Unreal's implementations are no-op passthroughs since none of its fields are ever dynamic.

### Adding a new Unreal game

1. Add `games/<key>.json` with `"engine": "unreal"` and an `engine_config` block, following `games/clair.json` as the template: `module` (leave `""` unless the game's SaveGame classes are native `/Script/...` classes, not Blueprints - see the portability gate section below), an `images` list (each entry needs `logical`, `save_name`, `pc_file`, and `payload`), and a `class_equivalence` list for any logical image whose PC and PS5 save classes differ (an image with no row isn't class-checked at all, and identical classes on both sides never need a row). `pc`/`ps5` in each row are matched by class-name suffix, not full path - see `games/clair.json`'s note for why.
2. `games/*.json` is embedded via `//go:embed` in this module's own `embed.go` (`Builtin`), so a consumer picking up a new module version gets it automatically; a consumer's own on-disk `--games-dir` overrides/extends the embedded ones by `game` key.
3. Add tests under `engine/unreal` exercising the new profile's `engine_config` the same way `unreal_test.go`'s `clairLikeConfig()` does — no new Go package needed unless the game needs logic the generic engine doesn't have yet.
4. Update the game's `docs/<game>.md` (in this module, if it's format-relevant) and any CLI-side docs in the consuming repo.

### Adding a new engine

A genuinely new save format needs a new `engine/<name>` package implementing `engine.Engine`, registered in `games/registry.go`'s `init()`.

## Larian (Baldur's Gate 3)

`engine/larian/lspk.go` implements the LSPK save-container format: parse a real `.lsv`'s header and entry table, read a named entry's raw on-disk bytes or zlib-decompressed content, `Repack` an unmodified archive back to bytes, and rebuild a modified one. `engine/larian/larian.go` wires this into a full `engine.Engine`: `Inspect`, dynamic filename resolution (`ResolvePayload`/`ResolvePCFile`, see above), and `ConvertToPS5`/`ConvertFromPS5`/`InstallOutputs`. What's still missing: an LSOF parser (`meta.lsf`), an Osiris parser (`StorySave.bin`), and mod-list parity checking against a save's Profile image (`modsettings.lsx`) — needed for the richer gate checks (`osiris-version`, `lsof-version`, `mod-parity`, `build-order`) from the original design spec, not for the graft itself.

**Format facts, independently confirmed against real PS5 and PC Baldur's Gate 3 saves** (format facts cross-referenced against [LSLib](https://github.com/Norbyte/lslib) for confirmation only — no code ported):

- 40-byte header: `LSPK` magic, `version u32` (only `18` supported), `fileListOffset u64`, `fileListSize u32`, `flags u8`, `priority u8`, `md5[16]`, `numParts u16` (only `1` supported - refused rather than mis-parsed, since no multi-part sample exists to develop against).
- The file list always spans to end-of-file (`fileListOffset + fileListSize == len(data)` held exactly on both real samples) - `Parse` refuses to guess if that invariant doesn't hold.
- The file list section is `numFiles u32` + `compressedSize u32` + a **raw LZ4 block** (not the LZ4 frame format - no header/footer, just the token/literal/match sequence loop) decompressing to `numFiles * 272` bytes of fixed-size entries: `name[256]` (null-terminated), `offsetLo u32` + `offsetHi u16` (giving a 48-bit file offset), `part u8`, `flags u8` (low nibble is the compression method: `0` none, `1` zlib, `2` lz4 - every entry observed on both platforms used zlib), `sizeOnDisk u32`, `uncompressedSize u32`.
- Go's stdlib has no LZ4 decoder, so `lz4BlockDecompress` in `lspk.go` is a from-scratch implementation of the public, well-documented raw-block algorithm (no third-party dependency added).
- `Repack` doesn't need an LZ4 *encoder* at all for the "nothing changed" case the round-trip test proves: it re-serializes the header from its parsed fields and copies everything else (all file content, and the original file-list table's compressed bytes) through verbatim. That's byte-identical to the input by construction whenever no field actually changed - confirmed against both real files (8.7 MB PS5 save, 29.4 MB PC save): `Parse` → `Repack` → `bytes.Equal(repacked, original)` held exactly, for every entry's raw and decompressed content too.
- `SaveInfo.json`'s `Platform` field is confirmed `"Prospero"` on the real PS5 save and `"Steam"` on the real PC save - exactly the string a real PS5↔PC conversion needs to rewrite.
- `Inspect` currently only checks LSPK magic/version/part-count and that the four required members (`meta.lsf`, `SaveInfo.json`, `StorySave.bin`, `Globals.lsf`) are present - both real saves pass. The richer BG3-specific gate checks (`osiris-version`, `lsof-version`, `mod-parity`, `build-order`) need those not-yet-built LSOF/Osiris parsers.

### Modifying and rebuilding archives: `WithReplacedEntry`, `Build`, and the writer requirements

Once a real conversion needs to change a file's content, an LZ4 *encoder* becomes necessary for the entry table, since offsets and sizes shift. `WithReplacedEntry` patches one entry in an existing archive; `Build` assembles a brand-new archive from an arbitrary `EntrySpec` set (the tool for grafting a different save's whole file set onto a container). Three writer requirements were established the hard way - each was a silent in-game rejection until found:

1. **64-byte entry alignment**: every packed entry (and the file list) is padded with `0xAD` bytes to a 64-byte boundary measured from the end of the header (matches LSLib's `PackageWriter.WritePadding`; confirmed on every real save). An unaligned but otherwise-valid archive is silently invisible to the game.
2. **The entry table's LZ4 must actually compress**: a valid-but-literal-only LZ4 encoding (output ≥ input) was also rejected. `encodeLZ4Block` is a real greedy match-finding raw-block encoder; `encodeLZ4LiteralOnly` survives only as its small-input fallback and as a test-fixture helper.
3. **Header `md5[16]`**: MD5 over every file's **uncompressed** content concatenated in **physical layout order**, then **every output byte incremented by 1**. The uncompressed/+1 parts come from LSLib's `PackageWriter.ComputeArchiveHash` (format facts only - no code ported); the physical-order part was pinned empirically, using a real game-written save's stored header MD5 as an oracle against twelve algorithm variants - exactly one matched. `MD5Recompute` is the strategy that matters; `MD5Unchanged`/`MD5Zero` remain only as experiment tooling.

### Confirmed working end-to-end: PC -> PS5 graft (2026-07-25)

A real 39-hour PC (Steam) save was successfully loaded on a real PS5 by rebuilding it with `Build()`: all entries from the PC `.lsv` (meta.lsf, StorySave.bin, Globals.lsf, LevelCache/*, WebP), `SaveInfo.json`'s `Platform` rewritten `"Steam"` -> `"Prospero"` (a targeted regex field rewrite preserving all other formatting), uploaded into a PS5 container under that container's existing filenames. Character/state loaded correctly in-game.

**The transport rules matter as much as the file format.** Getting to that result surfaced three independent transport failures, all now understood - these live in the *calling* CLI's transport layer (Garlic), not this module, but are recorded here since they shaped the graft mechanism:

- **The PS5 tracks saves in an OS-level savedata database** the game queries; raw file writes through Garlic don't touch registration. Writing `sce_sys/param.sfo` into a mounted container *breaks* its registration (Garlic scrubs `param.sfo` on export - `ACCOUNT_ID` zeroed, `SAVEDATA_DIRECTORY` rewritten to Garlic's image-file name - so re-importing an exported `param.sfo` mis-registers the container, producing doubled-name phantoms and hiding the save from the game). **Rule: never write anything under `sce_sys/`; only write into containers the game itself created.**
- **Garlic's HTTP API decodes `%20` but not `+` as a space in query strings.** Go's `url.Values.Encode()` uses `+`, so every request for a space-containing filename (all real BG3 save names) silently read/wrote a wrong, literally-`+`-named file.
- **Garlic zeroes `param.sfo` in mounted reads too**, so container metadata can't be inspected through this transport at all.

`ConvertToPS5`/`ConvertFromPS5` (`rebuildWithPlatform` in `engine/larian/larian.go`): parse → decompress every entry → rewrite `SaveInfo.json`'s `Platform` field via a targeted regex (preserves BG3's own formatting, not a JSON re-marshal) → `Build` with `MD5Recompute`.

`ConvertFromPS5` (PS5 -> PC, `Platform` `"Prospero"` -> `"Steam"`) uses the same mechanism in the opposite direction and is currently broken: every save tested hangs at 0% on PC load, both genuinely PS5-native content and content that's round-tripped Steam-native underneath, which rules out both "bad rebuild" and "PS5 content incompatible" as the sole explanation. Root cause unresolved. **See `docs/bg3.md`** for the full write-up: transport lessons, the three LSPK writer requirements, and the complete PS5→PC test log.

Known gaps: only one save image per BG3 profile is supported (`ConvertToPS5`/`ConvertFromPS5` both error on `len(images) != 1`); the `.WebP` thumbnail and the OS-level save-list display name/art aren't updated by a graft, so the save list may show stale text/art until the game next saves into that slot.

## Resident Evil 2

`reengine` decodes/re-encodes Capcom RE Engine's "DSSS" save container (Blowfish-LE cipher, murmur3 checksum) and parses/re-serializes its RSZ field data, including the alignment-base fix (RSZ field alignment is computed against the body's *absolute file offset*, not body-relative - a PC body sits at `0x20`, a PS5 body at `0x18`, so a verbatim body copy misaligns every 16-byte-aligned field). `engine/reengine` adapts that library to `engine.Engine` on the same dynamic-slot model BG3 uses (`games/re2.json`, title `PPSA04288`): the PS5 save slot isn't config-known, so the calling CLI's `--ps5-save-name` picks it, and the PC-side filename is *derived* from the resolved slot (`fileForSlot`) rather than discovered by glob, since one PC save directory holds every slot's file side by side. The global profile/settings slot (`data00-1.bin`) is refused outright - converting it crashed the game at startup.

**PC → PS5 conversion is confirmed working in-game** (two different real saves, both loaded correctly) - the library also retargets two platform-identity fields (`0x8b7dd7a1`'s `0xb41fa365`/`0xe231b945`) the same way BG3 rewrites `SaveInfo.json`'s `Platform` field. **PS5 → PC** is confirmed in-game too (see `TestCases.md`). **See `docs/ressave.md`** for the narrative investigation log (what was tried, in what order, what each result meant) and **`docs/dev-res2.md`** for the deep technical reference: full byte layouts, the conversion algorithm, and the complete eboot.bin disassembly (actual `objdump` listings, the ELF-location method, the crash-site/assert-routine/array-allocator trace, and the `klogsrv` crash-capture methodology).

### The wider RE Engine family (RE3/RE4/RE7/Village/Requiem)

Real PS5 saves were captured for all five and each was decoded/parsed directly rather than reasoned about. **See `TODO.md`'s "RE Engine family" section for the full findings breakdown** (which cipher each title/platform uses, key discovery, the RSZ alignment bit-mask bug, and per-title live-test confirmation). Headline result: **which cipher a save uses varies by title *and* platform**, and a title's PS5 save often doesn't match its own Steam save. RE3/RE4/RE7/Village/Requiem are all wired in (`games/re3.json`, `games/re4.json`, `games/re7.json`, `games/village.json`, `games/requiem.json`) and confirmed working in-game both directions.

## Subnautica

`engine/unityblb` implements a genuinely simpler save format than any other engine here: an unencrypted, gzip-wrapped flat sequence of `[1-byte name length][name][4-byte little-endian size][data]` entries, with no directory index, no proprietary class/versioning system, and no platform field anywhere in the decoded data (`blb.go`'s `Decode`/`Encode`). The PC side is a directory tree rather than a single file - `gameinfo.json`, `global-objects.bin`, `scene-objects.bin`, `screenshot.jpg`, and `CellsCache/*.zip` (one zip per world-cell "batch" the player has modified, bundling several individual cell `.bin` files, Stored/uncompressed). `cellscache.go` flattens a PC zip's members into individual container entries (PC→PS5) and regroups a container's flat per-cell entries back into per-batch-id zips (PS5→PC).

Because the PC side is a directory rather than one file, `gameapi.SaveImage.PCFile` holds a directory name here instead of a filename - the calling CLI's backup path needs to detect and recursively back up a directory-shaped `PCFile`. `InstallOutputs` also replaces a slot's PC directory wholesale rather than merging files in, so a stale `CellsCache` zip from a previously-explored save can't linger alongside a newly-converted one.

One portability check exists (`proto-version`, tier 2): `gameinfo.json`'s `protoBufVersion` field (real saves carry a leading UTF-8 BOM, stripped before parsing) must match between sides, since a mismatch means a different game build may not deserialize the save correctly.

Two profiles use this engine: `games/subnautica.json` (title `PPSA02453`, one static image, slot `slot0000`) and `games/subnautica_below_zero.json` (title `PPSA02457`, same engine and format, confirmed against a real save). Below Zero surfaced a real limitation worth knowing before adding a third title on this engine: **PS5 slot number and PC slot number don't have to match.** The base game's fixed `slot0000` <-> `slot0000` pairing works because this project's only save on either platform happens to occupy slot 0 on both - a coincidence, not a rule. Below Zero's PS5 side has exactly one save while the PC install has three slots; the profile's `pc_dir` is hardcoded to whichever slot actually corresponds (verified by matching `protoBufVersion` and near-identical `gameTime`), and there's no way to infer that pairing automatically the way BG3/RE2's `--ps5-save-name` resolves *their* ambiguity (that works because there's exactly one file to discover once you know the slot; here, all PC slots are equally valid candidates and only a human knows which one is the intended pair).

Confirmed byte-identical self-consistent round trip (both directions) against real PS5 saves and their independent real PC equivalents for both titles, plus a live end-to-end CLI run (both directions) against a real Garlic instance for both titles. **See `docs/subnautica.md`** for the full write-up.

## Portability Gate (Unreal)

This section covers Unreal's gate, the most developed one - the richest tier system and the only one with a `class_equivalence`-style learned-mapping table. The other engines have their own, lighter-weight checks in the same `Tier`/`Verdict` vocabulary: larian's `Inspect` checks LSPK magic/required-members only (see "Larian" above); reengine and unityblb each add one pairwise tier-2 check on top of that (see "Resident Evil 2" and "Subnautica" above).

Before any graft or write, `ConvertFromPS5`/`ConvertToPS5` run every applicable check against both sides of every required image (`engine/unreal/gate.go`). Each check has a tier:

- **Tier 3 (`magic`)** - the payload isn't a GVAS save at all. Never overridable.
- **Tier 2** - a specific, named check failed. Overridable one at a time by the calling CLI (`--allow <check>`):
  - `module` - the save class's Unreal module doesn't match `engine_config.module` (looks like a different game). Only meaningful for native `/Script/...` classes; leave `module` empty to disable this check for Blueprint-based SaveGame classes (`/Game/...`), which carry no reliable module-like signal - confirmed against Clair's real saves, whose two images live under two entirely unrelated `/Game/` content folders.
  - `account-id` - an embedded SteamID64 was found (account-bound save).
  - `account-props` - a property name suggests account binding (`steam`, `psn`, `account`, `uniquenetid`, `userid`, `epicid`).
  - `tail` - no `None\0` property-map terminator in the final 32 bytes (a trailer/checksum this library can't reproduce may be present).
  - `package-version` - UE4/UE5 package versions differ (also settable per-profile via `allow_package_version_mismatch: true`, once verified safe).
  - `class-map` - the (PC class, PS5 class) pair for a logical image isn't in `class_equivalence` and isn't identical on both sides.

If any required image fails a non-overridden check, the **whole run aborts before any write** - a partial graft across a multi-image game is worse than none.

**`class_equivalence` has three states**, not two: a row with `"verified": true` passes silently; `"verified": false` (a candidate) passes but always emits a warning and is recorded in the manifest; no matching row blocks. This lets a caller commit a not-yet-play-tested mapping without needing `--allow` on every run.

**Overrides** are the primary way to author a new game's `class_equivalence` table, not just a safety valve - you cannot learn a mapping works without grafting it once. Every check result (including which ones were overridden or are unverified candidates) belongs in the calling CLI's manifest, so a save that turns out subtly broken can be traced back to what was bypassed.

### Confirmed against real data

Real Clair saves on both sides (a real Steam PC save plus both PS5 users' saves) confirm:

- **Real save classes are full Blueprint content paths, not the `/Script/Sandfall.*` paths the class_equivalence table originally shipped with** (those were carried over from an illustrative example, never validated). Confirmed real classes:
  - `gameplay`: PC `/Game/Gameplay/Save/SaveObjects/BP_SaveGameObject_V8.BP_SaveGameObject_V8_C`, PS5 `/Game/Gameplay/Save/SaveObjects/BP_SaveGameObject_V7.BP_SaveGameObject_V7_C`.
  - `container`: PC and PS5 both `/Game/jRPGTemplate/Blueprints/SaveObjects/BP_jRPG_SavesContainer.BP_jRPG_SavesContainer_C` - byte-identical on both platforms.
  - `class_equivalence` matches by class-name suffix rather than full path, and `games/clair.json`'s `module` is `""` since Blueprint classes carry no reliable module-like signal (`gameplay` and `container` don't even share a content folder).
- `account-id`, `account-props`, and `tail` all **passed cleanly** on both real PS5 saves and the real PC save - no false positives observed, for either user.
- `class-map` and `package-version` (the pairwise checks, which need both a real PC and a real PS5 payload) both passed cleanly for both images, both users. `games/clair.json` has an explicit `verified: true` row for `container` too (rather than relying on the implicit identity-match), recording the confirmed real value in case a future game patch makes PC and PS5 diverge.

This doesn't mean every save state or every future game patch is covered - it's one save family, checked once. If a check ever false-positives on a save that used to work, `Inspect` shows exactly which one and why, and an override gets a caller past it for that run.
