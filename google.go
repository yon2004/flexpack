package main

// Google's cros-flex-tools CLI surface, reimplemented flag-for-flag:
//
//	package_flex_image  [-h] --image_path IMAGE_PATH --enrollment_token TOKEN
//	                    [--automate_setup] (--output OUTPUT | --in_place)
//	download_flex_image [-h] --image_type {usb,mass-deploy} [--output OUTPUT]
//
// Two ways in. Either as a subcommand:
//
//	flexpack package_flex_image --image_path x.bin --enrollment_token ... --in_place
//
// or by invocation name — copy or symlink the binary to package_flex_image
// (package_flex_image.exe on Windows) and existing scripts and runbooks work
// unchanged, no Debian and no WSL.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const packageUsage = `package_flex_image — package an enrolment token with a ChromeOS Flex image

  --image_path IMAGE_PATH        (required) image to package
  --enrollment_token TOKEN       (required) auto-enrolment token, lowercase UUID
  --automate_setup               automate OOBE setup steps after installation
  --output OUTPUT                write a packaged copy here
  --in_place                     modify IMAGE_PATH directly
  -h                             show this message

Exactly one of --output or --in_place is required.

Extensions beyond Google's tool:
  --config FILE                  use this config.json verbatim instead of --token
  --dry-run                      report what would happen, change nothing
  --force                        replace a config already present in the image
`

const downloadUsage = `download_flex_image — download a ChromeOS Flex image

  --image_type {usb,mass-deploy} (required) intended installation method
  --output OUTPUT                where to write the image
  -h                             show this message

Extensions beyond Google's tool:
  --channel CHANNEL              stable (default), lts, ltc, dev
  --no-verify                    skip the sha1/md5/size check
`

// multiCallName returns the Google tool this binary was invoked as, if any.
func multiCallName(argv0 string) string {
	base := strings.ToLower(filepath.Base(argv0))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "package_flex_image", "download_flex_image":
		return base
	}
	return ""
}

func cmdPackageFlexImage(args []string) error {
	fs := flag.NewFlagSet("package_flex_image", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, packageUsage) }

	imagePath := fs.String("image_path", "", "image to package")
	token := fs.String("enrollment_token", "", "auto-enrolment token")
	automate := fs.Bool("automate_setup", false, "automate OOBE setup steps")
	output := fs.String("output", "", "output path")
	inPlace := fs.Bool("in_place", false, "modify the input image directly")

	configFile := fs.String("config", "", "use this config.json verbatim")
	desc := fs.String("desc", "", "free-text note stored in the config's desc field")
	var sets kvFlag
	fs.Var(&sets, "set", "extra OOBE key, repeatable: --set skipHidScreen=true")
	dryRun := fs.Bool("dry-run", false, "change nothing")
	force := fs.Bool("force", false, "replace an existing config")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *imagePath == "" {
		fs.Usage()
		return fmt.Errorf("--image_path is required")
	}
	if (*token == "") == (*configFile == "") {
		fs.Usage()
		return fmt.Errorf("--enrollment_token is required (or --config to supply a config.json directly)")
	}
	// Google makes these mutually exclusive and requires one; match that
	// exactly so a script written against the real tool behaves identically.
	if *inPlace == (*output != "") {
		fs.Usage()
		return fmt.Errorf("give exactly one of --output or --in_place")
	}

	var data []byte
	var err error
	if *configFile != "" {
		data, err = LoadConfigFile(*configFile)
	} else {
		extra, perr := sets.Fields()
		if perr != nil {
			return perr
		}
		if *desc != "" {
			extra = append(extra, KV{"desc", *desc})
		}
		data, err = BuildConfig(*token, *automate, extra)
	}
	if err != nil {
		return err
	}

	work := *imagePath
	if *output != "" && !*dryRun {
		fmt.Fprintf(os.Stderr, "  copying %s -> %s\n", *imagePath, *output)
		if err := copyFile(*imagePath, *output); err != nil {
			return err
		}
		work = *output
	}

	return Inject(InjectOptions{
		ImagePath: work,
		Target:    flexConfigPath,
		Data:      data,
		Partition: "STATE",
		Force:     *force,
		DryRun:    *dryRun,
	})
}

func cmdDownloadFlexImage(args []string) error {
	fs := flag.NewFlagSet("download_flex_image", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, downloadUsage) }

	imageType := fs.String("image_type", "", "usb or mass-deploy")
	output := fs.String("output", "", "output path")
	channel := fs.String("channel", "stable", "stable, lts, ltc or dev")
	manifestURL := fs.String("manifest", DefaultManifestURL, "recovery manifest URL")
	noVerify := fs.Bool("no-verify", false, "skip verification")

	if err := fs.Parse(args); err != nil {
		return err
	}

	switch *imageType {
	case "usb":
		// The recovery manifest serves the USB installer image.
	case "mass-deploy":
		return fmt.Errorf("--image_type mass-deploy is not supported: the PXE/mass-deployment " +
			"image is not served from the recovery manifest this tool reads. " +
			"Use Google's download_flex_image for that one, or --image_type usb here")
	case "":
		fs.Usage()
		return fmt.Errorf("--image_type is required (usb or mass-deploy)")
	default:
		return fmt.Errorf("--image_type must be usb or mass-deploy, not %q", *imageType)
	}

	images, err := FetchManifest(*manifestURL)
	if err != nil {
		return err
	}
	image, err := SelectChannel(images, *channel)
	if err != nil {
		return err
	}

	out := *output
	if out == "" {
		// Google defaults to a name derived from the image type.
		out = "flex-image-usb.bin"
	}

	fmt.Fprintf(os.Stderr, "%s %s (%s)\n", image.Name, image.Version, image.Channel)
	fmt.Fprintf(os.Stderr, "  archive %s  ->  image %s\n",
		humanBytes(image.ZipFileSize), humanBytes(image.FileSize))

	return Fetch(image, out, !*noVerify)
}

// kvFlag collects repeated --set key=value pairs.
type kvFlag []string

func (k *kvFlag) String() string { return strings.Join(*k, ",") }

func (k *kvFlag) Set(v string) error {
	*k = append(*k, v)
	return nil
}

// Fields resolves each pair against the schema and coerces the value.
func (k *kvFlag) Fields() ([]KV, error) {
	var out []KV
	for _, pair := range *k {
		name, raw, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("--set expects key=value, got %q", pair)
		}
		name = strings.TrimSpace(name)

		spec, known := lookupKey(name)
		if !known {
			return nil, fmt.Errorf("%q is not a known OOBE configuration key; run 'flexpack keys' for the list", name)
		}
		v, err := CoerceValue(spec, strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		out = append(out, KV{name, v})
	}
	return out, nil
}
