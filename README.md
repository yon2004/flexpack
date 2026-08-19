# flexpack

Build ChromeOS Flex USB installers that auto-enrol into your Google Workspace
domain — on Windows, with no WSL, Crostini or Debian anywhere in the loop.

Google's official `cros-flex-tools` only runs on Debian, which means an IT tech
on a Windows laptop needs a Linux VM just to write sixty bytes of JSON into a
disk image. `flexpack` is a single 5 MB binary that does the same job natively,
using the same command-line flags, and produces byte-identical output.

Standard library only. No dependencies, no cgo, no install.

> Not affiliated with, endorsed by, or sponsored by Google. ChromeOS and
> ChromeOS Flex are trademarks of Google LLC.

---

## TL;DR

Four steps. Do steps 1 and 2 once a month; steps 3 and 4 per organisational
unit, in about a minute.

### 1. See what Google is serving

```
flexpack list
```

```
3 image(s) currently served:

  STABLE  16xxx.yy.0   chromeos_16xxx..._stable-channel_mp-v3.bin (1.1 GiB zip, 6.5 GiB image)
  LTS     15xxx.yy.0   ...
  DEV     17xxx.yy.0   ...
```

### 2. Download the base image — once

```
flexpack fetch --channel stable -o flex-base.bin
```

Streams and decompresses in a single pass, and verifies sha1, md5 and both
sizes against the manifest before the file is renamed into place. Takes about
as long as a 1.2 GB download; nothing else is the bottleneck.

**Keep this file.** The image is identical for every token — only the JSON
differs — so you never download it twice.

### 3. Inject the enrolment token

```
flexpack inject --image flex-base.bin --token 12345678-90ab-cdef-1234-567890abcdef --automate-setup
```

Modifies the image **in place** by default, so no 6.9 GB copy. Takes seconds.

Re-tagging the same image for a different OU just needs `--force`, which
rewrites the existing JSON in the block it already occupies:

```
flexpack inject --image flex-base.bin --token <other-uuid> --automate-setup --force
```

Get the token from the Admin console: **Devices → Chrome → Enrolment tokens**.
It must be a lowercase UUID; `flexpack` checks.

### 4. Write it to a USB stick

Any raw-image writer. On Windows, **Rufus in DD mode**:

- Rufus's file picker may hide `.bin` — rename it to `.img`
- Make sure it says **DD Image**, not ISO. ChromeOS's partition layout needs a
  byte-for-byte copy

Then boot the target device from the stick and install as normal. It enrols
with no credentials typed.

### If you want to keep several tagged images

Use `-o` instead, at the cost of 6.9 GB per copy:

```
flexpack inject --image flex-base.bin --token <uuid> --automate-setup -o finance.bin
```

### Prefer Google's exact syntax?

Every flag from `cros-flex-tools` works, so existing runbooks transfer:

```
flexpack download_flex_image --image_type usb --output flex-base.bin
flexpack package_flex_image  --image_path flex-base.bin \
                             --enrollment_token <uuid> --automate_setup --in_place
```

Copy the binary to `package_flex_image.exe` and it accepts those flags with no
subcommand at all — scripts written against the real tool run unchanged.

Note that `package_flex_image` requires you to pick `--output` or `--in_place`
explicitly, because Google's does. Only the shorter `flexpack inject` defaults
to in place.

---

## Install

```
go install github.com/yon2004/flexpack@latest
```

Or grab a binary from [Releases](../../releases). Windows binaries are
unsigned, so SmartScreen warns on first run — verify the SHA256 from the
release notes if that matters to you.

Building from source needs Go 1.21 or later:

```
go build -ldflags="-s -w" -o flexpack .
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o flexpack.exe .
```

## Security

**The enrolment token is a bearer credential.** Anyone holding the image or the
USB stick can read it out of the unencrypted STATE partition and enrol a device
into your organisation. Treat packaged images like secrets: do not commit them,
do not post them in chat, and wipe sticks you retire.

`.gitignore` excludes `*.bin`, `*.img` and `config.json` for this reason. A
committed config is a live token in public git history, and deleting it later
does not remove it from GitHub.

## Before you image thirty devices

The ext4 writer is validated on every build with `e2fsck`, against filesystems
created using ChromeOS's own mkfs parameters. That is very close to a genuine
Flex image but not identical to one.

```
flexpack inject --image flex-base.bin --token <uuid> --dry-run
```

`--dry-run` reports the partition offset and the filesystem feature flags it
found, and changes nothing. If it prints `block=4096 64bit=true
metadata_csum=true`, you are on the tested path.

**Then boot one device before making thirty sticks.**

## Command reference

### `flexpack list`

| Flag | Default | Meaning |
|---|---|---|
| `--manifest` | Google's Flex config | override the manifest URL |

### `flexpack fetch`

| Flag | Default | Meaning |
|---|---|---|
| `--channel` | `stable` | `stable`, `lts`, `ltc`, `dev` — whatever the manifest offers |
| `-o` | manifest filename | output path |
| `--manifest` | Google's Flex config | override the manifest URL |
| `--no-verify` | off | skip the hash and size checks |

A failed integrity check leaves nothing behind — the file is only renamed into
place once it verifies.

### `flexpack inject`

| Flag | Default | Meaning |
|---|---|---|
| `--image` | — | image to modify |
| `-o` | **in place** | write to a copy at this path instead |
| `--token` | — | enrolment token, lowercase UUID |
| `--config` | — | use an existing config.json instead of `--token` |
| `--automate-setup` | off | add the OOBE automation keys |
| `--force` | off | replace a config already present |
| `--dry-run` | off | report what would happen, change nothing |
| `--path` | `/unencrypted/flex_config/config.json` | target path inside the filesystem |
| `--partition` | `STATE` | GPT partition name |

### `flexpack parts` / `flexpack keys` / `flexpack version`

`parts --image x.bin` lists the GPT partitions. `keys` prints every OOBE
configuration key ChromeOS accepts (`--all` includes demo-mode and rollback
keys). `version` prints the build version.

## Why it's fast

The obvious approach writes the 1.2 GB archive to disk, reads it back, writes
the 6.9 GB image, then reads *that* back to modify it. Roughly 16 GB of disk
traffic to change sixty bytes. On a Chromebook over Crostini's 9p bridge that
is where the forty minutes goes.

`fetch` does one pass. Bytes come off the socket, through the SHA-1 and MD5
hashers, through the inflater, and land in the output file. Nothing else
touches the disk, and integrity verification is free because the hashers see
the compressed stream on its way past.

That makes the tool network-bound: the 1.2 GB download is the floor. And
because `inject` defaults to in place, re-tagging costs one block write.

## What it writes, and where

`/unencrypted/flex_config/config.json` on the STATE partition — the path
ChromeOS reads the Flex auto-enrolment token from, defined as
`kFlexOobeConfigUnencryptedFilePath` in
`oobe_config/filesystem/file_handler.h`.

On first boot the OS moves it into encrypted stateful, and deletes it after
enrolment completes.

## The config format

Confirmed against real packaging-tool output *and* against Chromium's
`chrome/browser/ash/login/configuration_keys.cc`:

```json
{"enrollmentToken": "...", "source": "PACKAGING_TOOL", "welcomeNext": true,
 "networkUseConnected": true, "eulaAutoAccept": true,
 "skipEnrollmentSuccessScreen": true}
```

Without `--automate-setup` only the first two keys are written. Field order
follows the real tool — `encoding/json` sorts map keys alphabetically, which
would scramble it, so the object is assembled by hand. `--automate_setup`
reproduces genuine output byte-for-byte, and `--config` round-trips a real
file byte-for-byte.

## Other OOBE keys

`flexpack keys` prints the full schema. Add them with repeatable `--set`:

```
flexpack inject --image flex-base.bin --token <uuid> --automate-setup \
  --set skipHidScreen=true --set language=en-AU \
  --desc "Finance OU - Melbourne"
```

Values are type-checked and unknown keys are rejected at the flag, so a typo
fails on your desk rather than on a device that quietly refuses to enrol.

### Two functions decide what a key does

`ValidateConfiguration` type-checks known keys and logs anything left over as
unknown — but leftovers do **not** invalidate the config; only a type mismatch
does. `FilterConfiguration` then builds the dictionary handed to the JS and C++
handlers by iterating `kAllConfigurationKeys`. A key missing from that table is
never copied across and reaches no handler at all.

### The landmine

`enrollmentAssetId` and `enrollmentAutoAttributes` are declared in the header,
documented in the .cc, and **absent from `kAllConfigurationKeys`**. The table
lists `kEnrollmentLocation` twice instead — once as STRING, once as BOOLEAN —
which is where those two entries should have been.

So both are filtered out and do nothing. They do not error. They do not warn
the operator. Asset tags simply never appear. `flexpack` flags them, and a test
asserts the bug still exists so the schema gets updated if upstream fixes it.

### Naming traps

- `skipHidScreen` — the constant is `kSkipHIDScreen`, the JSON key is not.
- `enrollmentRestoreAfterRollback` — the constant is `kRestoreAfterRollback`.
- `networkConfig` is documented as a rollback artefact, not a way to pre-seed
  Wi-Fi on a fresh install.
- `arcTosAutoAccept` does nothing on Flex; there is no ARC.
- `desc` is a free-text note. The validator knows it, so it raises no warning,
  and it is dropped before any handler sees it — a safe place to record which
  OU an image was built for.

## How it works

`manifest.go` fetches Google's recovery config, a plain-text file of
blank-line-separated `key=value` stanzas. A stanza counts as an image if it has
both `url` and `file`, which skips the `recovery_tool_version` preamble without
depending on its position.

`zipstream.go` parses the zip local file header directly. The archives are
zip64 — the image is past the 4 GB ceiling, which is what old `unzip` builds
mean by "need PK compat v4.5". Go's `archive/zip` handles zip64 correctly but
needs an `io.ReaderAt`, meaning the whole archive on disk first. Since these
archives hold one entry, reading the local header by hand lets us inflate
straight off the wire. Data descriptors and stored entries are handled too.

`fetch.go` wires it together: `io.TeeReader` feeds the hashers, an
`io.LimitReader` stops the inflater wandering into the central directory, and
the tail is drained afterwards so the hashes cover the whole archive.

`gpt.go` locates partitions by GPT name rather than type GUID, because
ChromeOS uses non-standard type GUIDs.

### Why the ext4 writer is hand-rolled

`go-diskfs` v1.6.0 reads a ChromeOS ext4 correctly but its writer is broken.
`Mkdir` returns success and corrupts the filesystem — it hands out an inode
that is already in use:

```
Entry 'newtop' in / (2) is a link to directory /marker.txt (15).
Directory inode 15, block #0, offset 0: directory corrupted
e2fsck: aborted
```

No error, corrupt image. That fails in the worst possible way: everything looks
fine until a device refuses to boot. So `ext4.go` implements the write directly
against the on-disk format, and `flexpack` stays dependency-free.

It is deliberately minimal and refuses anything it cannot do correctly rather
than guess: single-block files, depth-0 single-extent trees, no hashed (htree)
directories, no journal transaction (the filesystem is offline and we are the
only writer, same as `debugfs`), and allocation only in block groups mkfs
already initialised.

The checksum details that are easy to get wrong: metadata checksums are crc32c
seeded from the filesystem UUID; the group descriptor checksum zeroes
`bg_checksum` and hashes the descriptor contiguously (only the legacy crc16
path skips the field); directory blocks carry a 12-byte tail holding crc32c
over the inode number, generation and block body; and `s_desc_size` lives at
offset 0xFE, not 0xFC.

## Testing

```
go test ./...          # full suite
go test -short ./...   # skips the 8 GiB multi-block-group case
```

The filesystem tests build real ext4 images and validate every write with
`e2fsck`. They skip automatically where e2fsprogs is unavailable, so on Windows
and macOS you get the parser and schema tests only — CI runs the full set on
Linux.

`e2fsck` is the whole point of the suite. A writer that returns no error can
still leave a filesystem the kernel rejects; that is exactly how `go-diskfs`
fails. Only a clean `e2fsck` counts as a pass.

31 tests. Covered: populated parent directory, empty parent, neither directory
present, 8 GiB filesystem with 64 block groups, full GPT image with STATE at a
non-zero offset, in-place overwrite, re-tagging with `--force`, name clash
leaving no orphan inode, oversized file refused, dry run on an already-tagged
image, unknown partition naming what exists, zip64 and plain archives
byte-identical after a round trip, truncated and tampered archives caught.

## Licence and attribution

MIT. See `LICENSE`.

`schema.go` transcribes OOBE configuration key names, types and validation
semantics from Chromium's `chrome/browser/ash/login/configuration_keys.{h,cc}`
(BSD-style licence). `ext4.go` is an original implementation of the documented
ext4 on-disk format; its checksum routines reproduce the algorithms used by
e2fsprogs but contain no e2fsprogs code. See `NOTICE` for detail.
