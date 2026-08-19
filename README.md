# flexpack

Build ChromeOS Flex installer images with an auto-enrolment token baked in —
on Windows, with no WSL, Crostini or Debian anywhere in the loop.

Single binary, no dependencies outside the Go standard library, no cgo.

> Not affiliated with, endorsed by, or sponsored by Google. ChromeOS and
> ChromeOS Flex are trademarks of Google LLC.

## Install

```
go install github.com/YOURNAME/flexpack@latest
```

Or download a binary from [Releases](../../releases). Windows binaries are
unsigned, so SmartScreen will warn on first run — verify the SHA256 from the
release notes if that matters to you.

## Security

**The enrolment token is a bearer credential.** Anyone holding the image or
the USB stick can read it out of the unencrypted STATE partition and enrol a
device into your organisation. Treat packaged images like secrets: do not
commit them, do not post them in chat, and wipe sticks you retire.

`.gitignore` excludes `*.bin`, `*.img` and `config.json` for this reason.

## Status

The ext4 writer is validated on every build with `e2fsck` against filesystems
created using ChromeOS's own mkfs parameters. That is close to a genuine Flex
image but not identical to one. **Boot one device before imaging thirty.**

`flexpack inject --dry-run` reports the partition offset and filesystem
feature flags it found without changing anything — a good first move against
any image you have not used before.

## Why it's fast

The obvious approach writes the 1.2 GB archive to disk, reads it back, writes
the 6.9 GB image, then reads *that* back to modify it. Roughly 16 GB of disk
traffic to change sixty bytes.

`fetch` does one pass. Bytes come off the socket, through the SHA-1 and MD5
hashers, through the inflater, and land in the output file. Nothing else
touches the disk, and you get integrity verification for free because the
hashers see the compressed stream on its way past.

That makes the tool network-bound. The 1.2 GB download is the floor.

## Build

```
go build -ldflags="-s -w" -o flexpack .
```

Cross-compile for Windows:

```
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o flexpack.exe .
```

~4.9 MB. Go 1.21 or later.

## Use

```
flexpack list
flexpack fetch --channel stable -o flex-base.bin
```

`list` shows what Google is currently serving:

```
3 image(s) currently served:

  STABLE  16xxx.yy.0     chromeos_16xxx.yy.0_reven_recovery_stable-channel_mp-v3.bin (1.1 GiB zip, 6.5 GiB image)
  LTS     ...
  DEV     ...
```

`fetch` verifies the download against the manifest's `sha1`, `md5`,
`zipfilesize` and `filesize` before it renames the file into place. A failed
check leaves nothing behind. `--no-verify` skips it if you have a reason.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--channel` | `stable` | `stable`, `lts`, `ltc`, `dev` — whatever the manifest offers |
| `-o` | manifest filename | output path |
| `--manifest` | Google's Flex config | override the manifest URL |
| `--no-verify` | off | skip hash and size checks |

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
straight off the wire. Data descriptors and stored (uncompressed) entries are
handled too.

`fetch.go` wires it together: `io.TeeReader` feeds the hashers, an
`io.LimitReader` stops the inflater wandering into the central directory, and
the tail is drained afterwards so the hashes cover the whole archive.

## Tested

Against a local server with fixtures built by Info-ZIP:

- zip64 archive (`zip -fz`) → output byte-identical to the original
- plain zip archive → output byte-identical to the original
- truncated archive → fails during inflate, no partial file left
- tampered payload with intact zip structure → caught by sha1 and md5
- unknown channel → error naming the channels that do exist

## inject

```
flexpack inject --image flex-base.bin --token 12345678-90ab-cdef-1234-567890abcdef -o finance.bin
```

Finds the GPT partition named `STATE`, opens the ext4 inside it, and writes
`/unencrypted/flex_config/config.json` — the path ChromeOS reads the Flex
auto-enrolment token from, defined as `kFlexOobeConfigUnencryptedFilePath` in
`oobe_config/filesystem/file_handler.h`.

| Flag | Default | Meaning |
|---|---|---|
| `--image` | — | image to modify |
| `-o` | in place | copy to this path and modify the copy |
| `--token` | — | enrolment token, lowercase UUID |
| `--config` | — | use an existing config.json instead of `--token` |
| `--automate-setup` | off | add the OOBE automation keys (see caveat below) |
| `--force` | off | replace an existing config |
| `--dry-run` | off | report what would happen, change nothing |
| `--path` | `/unencrypted/flex_config/config.json` | target path inside the filesystem |
| `--partition` | `STATE` | GPT partition name |

Re-tagging an image for another OU uses `--force`, which rewrites the existing
data block in place. That path allocates nothing — no bitmaps or free counters
to get wrong — so it is the safest operation in the tool.

`flexpack parts --image x.bin` lists partitions if you need to look first.

### Why the ext4 writer is hand-rolled

`go-diskfs` v1.6.0 reads a ChromeOS ext4 correctly but its writer is broken.
`Mkdir` returns success and corrupts the filesystem — it hands out an inode
that is already in use:

```
Entry 'newtop' in / (2) is a link to directory /marker.txt (15).
Directory inode 15, block #0, offset 0: directory corrupted
e2fsck: aborted
```

No error, corrupt image. That fails in the worst possible way: everything
looks fine until a device refuses to boot. So `ext4.go` implements the write
directly against the on-disk format, and `flexpack` stays dependency-free.

It is deliberately minimal and refuses anything it cannot do correctly rather
than guess: single-block files, depth-0 single-extent trees, no hashed (htree)
directories, no journal transaction (the filesystem is offline and we are the
only writer, same as debugfs), and allocation only in block groups mkfs
already initialised.

### Validated

Every case below checked with `e2fsck -fn` **and** a real kernel mount:

- parent directory with existing entries
- empty parent directory
- neither parent nor target directory present (both created)
- 8 GiB filesystem, 64 block groups
- full GPT image, STATE at a non-zero partition offset
- `--force` in-place replacement
- name clash without `--force` — refused, and leaves no orphan inode

The checksum details that matter, since they are easy to get wrong: metadata
checksums are crc32c seeded from the filesystem UUID; the group descriptor
checksum zeroes `bg_checksum` and hashes the descriptor contiguously (only the
legacy crc16 path skips the field); directory blocks carry a 12-byte tail
holding crc32c over the inode number, generation and block body.

## Google-compatible CLI

The same flags as `cros-flex-tools`, so existing runbooks and scripts transfer:

```
flexpack download_flex_image --image_type usb --output flex-base.bin
flexpack package_flex_image  --image_path flex-base.bin \
                             --enrollment_token 12345678-90ab-cdef-1234-567890abcdef \
                             --automate_setup --output finance.bin
```

`--output` and `--in_place` are mutually exclusive and one is required, matching
Google's tool exactly.

**Multi-call.** Copy or symlink the binary to `package_flex_image`
(`package_flex_image.exe` on Windows) and it accepts Google's flags directly
with no subcommand:

```
copy flexpack.exe package_flex_image.exe
package_flex_image --image_path flex-base.bin --enrollment_token <uuid> --in_place
```

Scripts written against the real tool then run unchanged on Windows — no
Debian, no Crostini, no WSL.

`--image_type mass-deploy` is rejected with an explanation rather than silently
returning the wrong image: the PXE/mass-deployment image is not served from the
recovery manifest this tool reads.

Extensions beyond Google's surface: `--config FILE` (use a config.json
verbatim), `--dry-run`, `--force`, and `--channel` on the download side.

## The config format

Confirmed against real packaging-tool output *and* against Chromium's
`chrome/browser/ash/login/configuration_keys.cc`:

```json
{"enrollmentToken": "...", "source": "PACKAGING_TOOL", "welcomeNext": true,
 "networkUseConnected": true, "eulaAutoAccept": true,
 "skipEnrollmentSuccessScreen": true}
```

Without `--automate_setup` only the first two keys are written. Field order
follows the real tool — `encoding/json` sorts map keys alphabetically, which
would scramble it, so the object is assembled by hand. `package_flex_image
--automate_setup` reproduces genuine output byte-for-byte, and `--config`
round-trips a real file byte-for-byte.

## Other keys

`flexpack keys` prints the full schema. Add them with repeatable `--set`:

```
flexpack package_flex_image --image_path base.bin --enrollment_token <uuid> \
  --automate_setup --set skipHidScreen=true --set language=en-AU \
  --desc "Finance OU - Melbourne" --in_place
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
the operator. Asset tags simply never appear. `flexpack` flags them.

### Naming traps

- `skipHidScreen` — the constant is `kSkipHIDScreen`, the JSON key is not.
- `enrollmentRestoreAfterRollback` — the constant is `kRestoreAfterRollback`.
- `networkConfig` is documented as a rollback artefact, not a way to pre-seed
  Wi-Fi on a fresh install.
- `arcTosAutoAccept` does nothing on Flex; there is no ARC.
- `desc` is a free-text note. The validator knows it, so it raises no warning,
  and it is dropped before any handler sees it — a safe place to record which
  OU an image was built for.

## Testing

```
go test ./...          # full suite
go test -short ./...   # skips the 8 GiB multi-block-group case
```

The filesystem tests build real ext4 images and validate every write with
`e2fsck`. They skip automatically where e2fsprogs is unavailable, so on
Windows and macOS you get the parser and schema tests only — CI runs the full
set on Linux.

`e2fsck` is the whole point of the suite. A writer that returns no error can
still leave a filesystem the kernel rejects; that is exactly how `go-diskfs`
fails. Only a clean `e2fsck` counts as a pass.

## Licence and attribution

MIT. See `LICENSE`.

`schema.go` transcribes OOBE configuration key names, types and validation
semantics from Chromium's `chrome/browser/ash/login/configuration_keys.{h,cc}`
(BSD-style licence). `ext4.go` is an original implementation of the documented
ext4 on-disk format; its checksum routines reproduce the algorithms used by
e2fsprogs but contain no e2fsprogs code. See `NOTICE` for detail.
