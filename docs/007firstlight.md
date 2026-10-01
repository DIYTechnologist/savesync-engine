# 007 First Light (`glacier` engine)

## Layout

Both platforms use the same slot names. On PS5 each is a Garlic save image (`sdimg_<Name>`); on Steam each is a folder under `userdata/<account-id>/3768760/remote/`, lowercased by Steam Cloud:

| PS5 image | Steam folder | Contents |
|---|---|---|
| `sdimg_KntSlotSaveFile-0` | `kntslotsavefile-0` | save slot 1 |
| `sdimg_KntProfileSaveFile` | `kntprofilesavefile` | profile / progression |
| `sdimg_KntProfileSaveFile-BCK-0`, `-BCK-1` | `kntprofilesavefile-bck-0`, `-bck-1` | profile backups |
| `sdimg_SystemData` | `systemdata` | settings - not converted |
| `sdimg_LocalProfile` | `localprofile` | per-platform profile - not converted |

Every slot holds two files:

- `index.save` - a typed, self-describing header. It starts `03 00 00 00 01 0f 00 00 80 "SSaveGameHeader"` followed by a type table (`uint32`, `TArray<ERequirementId>`, `ZString`, `ESaveType`, ...).
- `data.save` - zlib-compressed game state, in the same typed serialization (`ZDynamicObject`, `SDynamicObjectKeyValuePair`, ...).

## Platform difference

The PS5 files are plaintext. Each Steam file is the same bytes XORed with the account's SteamID64, written little-endian and repeated every 8 bytes from offset 0. Nothing else differs, so conversion is just that XOR. No PS5 user ID or Steam ID appears inside the payloads.

Because `index.save` has a known plaintext prefix, the key (and so the owning SteamID64) can be recovered from any Steam `index.save`. The engine uses this to tell the user which account a save belongs to when `--steam-id` doesn't match.

## Header fields (observed, not fully understood)

Offsets in a plaintext `index.save`, all little-endian:

| Offset | Size | Meaning |
|---|---|---|
| 0x129 | 4 | game version stamp: `1001000` on PC 1.1.0, `1001001` on PS5 1.1.1 |
| 0x12d | 4 | hash, likely over the payload - not CRC32/CRC32C/Adler32/FNV/Murmur3/xxHash |
| 0x131 | 4 | uncompressed size of `data.save` |
| 0x135 | 4 | unix timestamp of the save |

The engine passes all of them through untouched. The hash stays valid because the payload bytes don't change. Editing save contents would need this hash identified first.

A PC 1.1.0 save loaded fine on PS5 1.1.1. Moving a newer PS5 save to an older PC build is untested; keep both versions in step if it fails to load.

## Notes

- After `--install` on PC, Steam Cloud may report a sync conflict for the replaced files; keep the local copy.
- PS5 images can't be created by this tool - save once in-game on the console so the slot images exist.
