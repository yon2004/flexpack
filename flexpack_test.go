package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

// -------------------------------------------------------------- manifest

const sampleManifest = `recovery_tool_version=0.9.2
recovery_tool_linux_version=0.9.2
recovery_tool_update=

name=ChromeOS Flex
version=14794.0.0
desc=
channel=DEV
hwidmatch=^REVEN($|-.*)
hwid=
md5=442e88dec2cf2201c82c60cc6ee9a8f6
sha1=93ebd1cf8c3463e668cc507f1b165ced6791831e
zipfilesize=1174071023
file=chromeos_14794.0.0_reven_recovery_dev-channel_mp-v2.bin
filesize=6939566592
url=https://dl.google.com/dl/edgedl/chromeos/recovery/chromeos_14794.0.0_reven.bin.zip

name=ChromeOS Flex
version=15117.112.0
channel=STABLE
md5=23cc0c6a0c0976e626e15f08b03a8691
sha1=99dd29cbc92df4a72f5c65348764a0b83a0f2bd5
zipfilesize=1206930016
file=chromeos_15117.112.0_reven_recovery_stable-channel_mp-v2.bin
filesize=6939566592
url=https://dl.google.com/dl/edgedl/chromeos/recovery/chromeos_15117.112.0_reven.bin.zip
`

func TestParseManifestSkipsPreamble(t *testing.T) {
	images, err := ParseManifest(strings.NewReader(sampleManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// The recovery_tool_* stanza has no url=, so it must not become an image.
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
	if images[0].Channel != "DEV" || images[1].Channel != "STABLE" {
		t.Errorf("channels = %q, %q", images[0].Channel, images[1].Channel)
	}
	if images[1].FileSize != 6939566592 {
		t.Errorf("FileSize = %d", images[1].FileSize)
	}
	if images[1].ZipFileSize != 1206930016 {
		t.Errorf("ZipFileSize = %d", images[1].ZipFileSize)
	}
	if images[1].SHA1 != "99dd29cbc92df4a72f5c65348764a0b83a0f2bd5" {
		t.Errorf("SHA1 = %q", images[1].SHA1)
	}
}

func TestSelectChannelIsCaseInsensitive(t *testing.T) {
	images, _ := ParseManifest(strings.NewReader(sampleManifest))
	for _, in := range []string{"stable", "STABLE", "Stable", " stable "} {
		got, err := SelectChannel(images, in)
		if err != nil {
			t.Fatalf("SelectChannel(%q): %v", in, err)
		}
		if got.Version != "15117.112.0" {
			t.Errorf("SelectChannel(%q) = %q", in, got.Version)
		}
	}
}

func TestSelectChannelErrorListsAvailable(t *testing.T) {
	images, _ := ParseManifest(strings.NewReader(sampleManifest))
	_, err := SelectChannel(images, "lts")
	if err == nil {
		t.Fatal("expected an error for a missing channel")
	}
	// The error should tell the operator what they can pick instead.
	if !strings.Contains(err.Error(), "DEV") || !strings.Contains(err.Error(), "STABLE") {
		t.Errorf("error does not list available channels: %v", err)
	}
}

func TestParseManifestRejectsEmpty(t *testing.T) {
	if _, err := ParseManifest(strings.NewReader("junk\nwith=no urls\n")); err == nil {
		t.Fatal("expected an error when no stanza has url= and file=")
	}
}

// -------------------------------------------------------------- zipstream

// buildLocalHeader assembles a zip local file header for testing.
func buildLocalHeader(name string, flags, method uint16, comp, uncomp uint32, extra []byte) []byte {
	h := make([]byte, localHeaderLen)
	binary.LittleEndian.PutUint32(h[0:], localHeaderSignature)
	binary.LittleEndian.PutUint16(h[6:], flags)
	binary.LittleEndian.PutUint16(h[8:], method)
	binary.LittleEndian.PutUint32(h[18:], comp)
	binary.LittleEndian.PutUint32(h[22:], uncomp)
	binary.LittleEndian.PutUint16(h[26:], uint16(len(name)))
	binary.LittleEndian.PutUint16(h[28:], uint16(len(extra)))
	h = append(h, name...)
	return append(h, extra...)
}

func TestReadLocalHeaderPlain(t *testing.T) {
	raw := buildLocalHeader("image.bin", 0, methodDeflate, 1000, 5000, nil)
	e, err := ReadLocalHeader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ReadLocalHeader: %v", err)
	}
	if e.Name != "image.bin" || e.Method != methodDeflate {
		t.Errorf("Name=%q Method=%d", e.Name, e.Method)
	}
	if e.CompressedSize != 1000 || e.UncompressedSize != 5000 {
		t.Errorf("sizes = %d/%d", e.CompressedSize, e.UncompressedSize)
	}
}

// The Flex images are past the 4 GB ceiling, so this is the path that matters.
func TestReadLocalHeaderZip64(t *testing.T) {
	extra := make([]byte, 20)
	binary.LittleEndian.PutUint16(extra[0:], zip64ExtraID)
	binary.LittleEndian.PutUint16(extra[2:], 16)
	binary.LittleEndian.PutUint64(extra[4:], 6939566592)  // uncompressed
	binary.LittleEndian.PutUint64(extra[12:], 1206930016) // compressed

	raw := buildLocalHeader("big.bin", 0, methodDeflate, 0xFFFFFFFF, 0xFFFFFFFF, extra)
	e, err := ReadLocalHeader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ReadLocalHeader: %v", err)
	}
	if e.UncompressedSize != 6939566592 {
		t.Errorf("UncompressedSize = %d, want 6939566592", e.UncompressedSize)
	}
	if e.CompressedSize != 1206930016 {
		t.Errorf("CompressedSize = %d, want 1206930016", e.CompressedSize)
	}
}

func TestReadLocalHeaderDataDescriptor(t *testing.T) {
	raw := buildLocalHeader("x.bin", flagDataDescriptor, methodDeflate, 0, 0, nil)
	e, err := ReadLocalHeader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ReadLocalHeader: %v", err)
	}
	if !e.HasDataDescriptor {
		t.Error("HasDataDescriptor = false")
	}
	// Sizes in the header are meaningless when a descriptor follows the data.
	if e.CompressedSize != sizeUnknown || e.UncompressedSize != sizeUnknown {
		t.Errorf("sizes should be unknown, got %d/%d", e.CompressedSize, e.UncompressedSize)
	}
}

func TestReadLocalHeaderRejectsNonZip(t *testing.T) {
	if _, err := ReadLocalHeader(strings.NewReader("not a zip file at all......")); err == nil {
		t.Fatal("expected a signature error")
	}
}

// -------------------------------------------------------------- config

func TestBuildConfigMatchesToolFieldOrder(t *testing.T) {
	got, err := BuildConfig("12345678-90ab-cdef-1234-567890abcdef", true, nil)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	want := `{"enrollmentToken": "12345678-90ab-cdef-1234-567890abcdef", ` +
		`"source": "PACKAGING_TOOL", "welcomeNext": true, "networkUseConnected": true, ` +
		`"eulaAutoAccept": true, "skipEnrollmentSuccessScreen": true}`
	if string(got) != want {
		t.Errorf("output does not match real packaging-tool output.\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildConfigWithoutAutomateSetup(t *testing.T) {
	got, _ := BuildConfig("12345678-90ab-cdef-1234-567890abcdef", false, nil)
	want := `{"enrollmentToken": "12345678-90ab-cdef-1234-567890abcdef", "source": "PACKAGING_TOOL"}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestBuildConfigNormalisesAndValidatesToken(t *testing.T) {
	got, err := BuildConfig("12345678-90AB-CDEF-1234-567890ABCDEF", false, nil)
	if err != nil {
		t.Fatalf("uppercase token should be normalised, not rejected: %v", err)
	}
	if !strings.Contains(string(got), "12345678-90ab-cdef-1234-567890abcdef") {
		t.Errorf("token not lowercased: %s", got)
	}

	for _, bad := range []string{"", "not-a-uuid", "12345678-90ab-cdef-1234-567890abcdeg"} {
		if _, err := BuildConfig(bad, false, nil); err == nil {
			t.Errorf("BuildConfig(%q) should have failed", bad)
		}
	}
}

// The two keys Chromium declares but never registers must be flagged. If this
// test ever starts failing, check whether upstream fixed the table.
func TestValidateConfigFlagsUnregisteredKeys(t *testing.T) {
	for _, key := range []string{"enrollmentAssetId", "enrollmentAutoAttributes"} {
		spec, ok := lookupKey(key)
		if !ok {
			t.Fatalf("%s missing from schema", key)
		}
		if spec.Registered {
			t.Errorf("%s marked Registered; it is absent from kAllConfigurationKeys", key)
		}

		var val any = "x"
		if spec.Type == typeBool {
			val = true
		}
		diags, fatal := ValidateConfig(map[string]any{key: val})
		if fatal {
			t.Errorf("%s should warn, not fail: unknown keys do not invalidate a config", key)
		}
		found := false
		for _, d := range diags {
			if d.Level == "warning" && strings.Contains(d.Msg, "filtered out") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s produced no dropped-key warning: %v", key, diags)
		}
	}
}

func TestValidateConfigTypeMismatchIsFatal(t *testing.T) {
	_, fatal := ValidateConfig(map[string]any{"welcomeNext": "true"})
	if !fatal {
		t.Error("a string in a boolean key should be fatal — Chromium returns valid=false")
	}
	_, fatal = ValidateConfig(map[string]any{"enrollmentToken": true})
	if !fatal {
		t.Error("a boolean in a string key should be fatal")
	}
}

func TestValidateConfigUnknownKeyWarnsOnly(t *testing.T) {
	diags, fatal := ValidateConfig(map[string]any{"updateSkipUpdate": true})
	if fatal {
		t.Error("unknown keys must not be fatal; Chromium only logs a warning")
	}
	if len(diags) == 0 {
		t.Error("unknown key produced no diagnostic")
	}
}

func TestRealToolOutputValidatesClean(t *testing.T) {
	cfg := map[string]any{
		"enrollmentToken":             "12345678-90ab-cdef-1234-567890abcdef",
		"source":                      "PACKAGING_TOOL",
		"welcomeNext":                 true,
		"networkUseConnected":         true,
		"eulaAutoAccept":              true,
		"skipEnrollmentSuccessScreen": true,
	}
	diags, fatal := ValidateConfig(cfg)
	if fatal {
		t.Fatal("genuine packaging-tool output must validate")
	}
	for _, d := range diags {
		if d.Level != "note" {
			t.Errorf("unexpected %s on real output: %v", d.Level, d)
		}
	}
}

func TestCoerceValue(t *testing.T) {
	boolSpec, _ := lookupKey("skipHidScreen")
	for _, in := range []string{"true", "1", "yes"} {
		if v, err := CoerceValue(boolSpec, in); err != nil || v != true {
			t.Errorf("CoerceValue(%q) = %v, %v", in, v, err)
		}
	}
	if _, err := CoerceValue(boolSpec, "yeah"); err == nil {
		t.Error("non-boolean text should be rejected for a boolean key")
	}

	strSpec, _ := lookupKey("language")
	if v, _ := CoerceValue(strSpec, "en-AU"); v != "en-AU" {
		t.Errorf("string coercion changed the value: %v", v)
	}
}

// Every schema entry should be internally consistent.
func TestSchemaWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range oobeKeys {
		if seen[k.Key] {
			t.Errorf("duplicate schema entry for %q", k.Key)
		}
		seen[k.Key] = true
		if k.Desc == "" {
			t.Errorf("%q has no description", k.Key)
		}
		if k.EmittedByTool && !k.Registered {
			t.Errorf("%q is emitted by the tool but marked unregistered", k.Key)
		}
	}
	for _, k := range []string{"enrollmentToken", "source", "welcomeNext",
		"networkUseConnected", "eulaAutoAccept", "skipEnrollmentSuccessScreen"} {
		spec, ok := lookupKey(k)
		if !ok || !spec.EmittedByTool {
			t.Errorf("%q should be marked as emitted by --automate_setup", k)
		}
	}
}

// TestSetFlagsReachInject guards the flag wiring: --set and --desc were
// documented for `flexpack inject` but only implemented on
// `package_flex_image`, so they parsed nowhere and silently did nothing.
func TestSetFlagsProduceFields(t *testing.T) {
	var sets kvFlag
	for _, s := range []string{"skipHidScreen=true", "language=en-AU"} {
		if err := sets.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	fields, err := sets.Fields()
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(fields))
	}
	if fields[0].Key != "skipHidScreen" || fields[0].Value != true {
		t.Errorf("boolean not coerced: %+v", fields[0])
	}
	if fields[1].Value != "en-AU" {
		t.Errorf("string mangled: %+v", fields[1])
	}

	data, err := BuildConfig("12345678-90ab-cdef-1234-567890abcdef", true,
		append(fields, KV{"desc", "Finance OU"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"skipHidScreen": true`, `"language": "en-AU"`, `"desc": "Finance OU"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config missing %s:\n%s", want, data)
		}
	}
}

func TestSetFlagRejectsUnknownKey(t *testing.T) {
	var sets kvFlag
	_ = sets.Set("updateSkipUpdate=true") // plausible, but not a real key
	if _, err := sets.Fields(); err == nil {
		t.Fatal("an unknown key must be rejected at the flag")
	}
}
