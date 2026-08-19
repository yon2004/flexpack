package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// The path ChromeOS reads the Flex auto-enrolment token from. Defined in
// ChromiumOS as kFlexOobeConfigUnencryptedFilePath in
// oobe_config/filesystem/file_handler.h. On first boot the OS moves it to
// encrypted stateful and deletes it from here.
const flexConfigPath = "/unencrypted/flex_config/config.json"

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// InjectOptions controls a single injection.
type InjectOptions struct {
	ImagePath string
	Target    string // path inside the filesystem
	Data      []byte
	Partition string // GPT partition name, normally "STATE"
	Force     bool   // overwrite an existing file
	DryRun    bool
}

// Inject writes Data into the named partition's ext4 filesystem.
func Inject(o InjectOptions) error {
	flags := os.O_RDWR
	if o.DryRun {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(o.ImagePath, flags, 0)
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	defer f.Close()

	parts, err := ReadGPT(f, 512)
	if err != nil {
		return err
	}

	p, ok := FindPartition(parts, o.Partition)
	if !ok {
		var names []string
		for _, q := range parts {
			names = append(names, q.Name)
		}
		return fmt.Errorf("no partition named %q; image has: %s",
			o.Partition, strings.Join(names, ", "))
	}
	fmt.Fprintf(os.Stderr, "  partition %d %q at offset %d (%s)\n",
		p.Index, p.Name, p.Start, humanBytes(p.Length))

	fs, err := OpenFS(f, p.Start)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  ext4 label=%q block=%d groups=%d 64bit=%v metadata_csum=%v\n",
		fs.Label(), fs.blockSize, fs.groups, fs.is64Bit, fs.metadataCsum)

	dir, base := path.Split(path.Clean(o.Target))
	dirParts := splitPath(dir)

	// Does the target already exist?
	existing, err := fs.Resolve(append(append([]string{}, dirParts...), base))
	if err != nil {
		return err
	}
	// A dry run reports; it never fails on state it is only describing.
	// Checking Force first would make --dry-run error out on exactly the
	// images an operator most wants to inspect.
	if o.DryRun {
		switch {
		case existing != nil && !o.Force:
			fmt.Fprintf(os.Stderr, "  dry run: %s already exists (inode %d) — would need --force\n",
				o.Target, existing.num)
		case existing != nil:
			fmt.Fprintf(os.Stderr, "  dry run: would replace %s in place (inode %d)\n",
				o.Target, existing.num)
		default:
			fmt.Fprintf(os.Stderr, "  dry run: would create %s\n", o.Target)
		}
		return nil
	}

	if existing != nil && !o.Force {
		return fmt.Errorf("%s already exists in this image (inode %d); pass --force to replace it",
			o.Target, existing.num)
	}

	// Replacing in place allocates nothing, so prefer it when we can.
	if existing != nil {
		if err := fs.Overwrite(existing, o.Data); err != nil {
			return err
		}
		if err := fs.Sync(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  replaced %s in place (%d bytes)\n", o.Target, len(o.Data))
		return nil
	}

	// Walk down, creating directories that are missing.
	cur, err := fs.Resolve(nil)
	if err != nil {
		return err
	}
	for i, name := range dirParts {
		next, err := fs.Resolve(dirParts[:i+1])
		if err != nil {
			return err
		}
		if next == nil {
			if next, err = fs.Mkdir(cur, name); err != nil {
				return fmt.Errorf("creating directory %q: %w", name, err)
			}
			fmt.Fprintf(os.Stderr, "  created %s/ (inode %d)\n", strings.Join(dirParts[:i+1], "/"), next.num)
		}
		cur = next
	}

	in, err := fs.CreateFile(cur, base, o.Data)
	if err != nil {
		return fmt.Errorf("creating %s: %w", o.Target, err)
	}
	if err := fs.Sync(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  wrote %s (inode %d, %d bytes)\n", o.Target, in.num, len(o.Data))
	return nil
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// The key set below is taken from real packaging-tool output rather than
// guessed. Observed:
//
//	{"enrollmentToken": "...", "source": "PACKAGING_TOOL", "welcomeNext": true,
//	 "networkUseConnected": true, "eulaAutoAccept": true,
//	 "skipEnrollmentSuccessScreen": true}
//
// Note what is absent: no eulaSendStatistics, no updateSkipUpdate, no ARC
// keys. Chrome's OOBE configuration schema defines many more keys, but the
// packaging tool only emits these.
const sourcePackagingTool = "PACKAGING_TOOL"

// automateSetupKeys are the keys --automate_setup adds, in the order the real
// tool writes them. Order carries no meaning in JSON, but matching it makes a
// byte-level diff against real output readable.
var automateSetupKeys = []struct {
	Key   string
	Value any
}{
	{"welcomeNext", true},
	{"networkUseConnected", true},
	{"eulaAutoAccept", true},
	{"skipEnrollmentSuccessScreen", true},
}

// BuildConfig produces the config.json body for a token.
//
// Field order follows the real tool so `diff` against genuine output shows
// only genuine differences. encoding/json sorts map keys alphabetically, which
// would scramble it, so the object is assembled by hand.
func BuildConfig(token string, automateSetup bool, extra []KV) ([]byte, error) {
	t := strings.ToLower(strings.TrimSpace(token))
	if t == "" {
		return nil, fmt.Errorf("an enrolment token is required")
	}
	if !uuidRE.MatchString(t) {
		return nil, fmt.Errorf("token %q is not a lowercase UUID; "+
			"expected 8-4-4-4-12 hex, e.g. 12345678-90ab-cdef-1234-567890abcdef", token)
	}

	fields := []KV{
		{"enrollmentToken", t},
		{"source", sourcePackagingTool},
	}
	if automateSetup {
		for _, k := range automateSetupKeys {
			fields = append(fields, KV{k.Key, k.Value})
		}
	}
	fields = append(fields, extra...)

	// Run the same checks Chromium will, so problems surface here rather than
	// on a device that quietly fails to enrol.
	probe := map[string]any{}
	for _, f := range fields {
		probe[f.Key] = f.Value
	}
	diags, fatal := ValidateConfig(probe)
	for _, d := range diags {
		fmt.Fprintf(os.Stderr, "  %s\n", d)
	}
	if fatal {
		return nil, fmt.Errorf("config would be rejected by Chromium's ValidateConfiguration")
	}

	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteString(", ")
		}
		k, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(f.Value)
		if err != nil {
			return nil, fmt.Errorf("encoding %q: %w", f.Key, err)
		}
		b.Write(k)
		b.WriteString(": ")
		b.Write(v)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// KV is one ordered config field.
type KV struct {
	Key   string
	Value any
}

// LoadConfigFile reads a config.json produced elsewhere (the HTML builder, or
// the real package_flex_image) and checks it parses.
func LoadConfigFile(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", p, err)
	}
	if tok, ok := probe["enrollmentToken"].(string); ok && !uuidRE.MatchString(tok) {
		fmt.Fprintf(os.Stderr, "  warning: enrollmentToken %q is not a lowercase UUID\n", tok)
	}

	diags, fatal := ValidateConfig(probe)
	for _, d := range diags {
		fmt.Fprintf(os.Stderr, "  %s\n", d)
	}
	if fatal {
		return nil, fmt.Errorf("%s would be rejected by Chromium's ValidateConfiguration", p)
	}
	return b, nil
}
