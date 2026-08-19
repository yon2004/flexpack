package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// version is set at build time via -ldflags.
var version = "dev"

const usage = `flexpack — build ChromeOS Flex installer images with an enrolment token baked in

Usage:
  flexpack list  [--manifest URL]
  flexpack fetch [--channel stable] [-o image.bin] [--no-verify] [--manifest URL]

Commands:
  list    Show the images Google is currently serving.
  fetch   Download an image, inflating straight to disk in a single pass,
          and verify it against the manifest's sha1, md5 and sizes.

  keys    List every OOBE configuration key Chromium accepts.
  version Print the build version.
  parts   List the partitions in an image.
  inject  Write a Flex enrolment config into the image's STATE partition.

Typical run:
  flexpack fetch  --channel stable -o flex-base.bin
  flexpack inject --image flex-base.bin --token 12345678-90ab-cdef-1234-567890abcdef -o finance.bin

Google-compatible commands (same flags as cros-flex-tools):
  download_flex_image --image_type usb [--output OUTPUT]
  package_flex_image  --image_path IMAGE --enrollment_token TOKEN
                      [--automate_setup] (--output OUTPUT | --in_place)

Copy or symlink this binary to package_flex_image (or .exe) and it accepts
Google's flags directly, so existing scripts work unchanged.

Run 'flexpack <command> -h' for the flags of a single command.
`

func main() {
	// Multi-call: copying or symlinking the binary to package_flex_image or
	// download_flex_image makes it accept Google's flags directly, so scripts
	// written against cros-flex-tools work unchanged.
	if name := multiCallName(os.Args[0]); name != "" {
		var err error
		switch name {
		case "keys":
			err = cmdKeys(os.Args[2:])
		case "package_flex_image":
			err = cmdPackageFlexImage(os.Args[1:])
		case "download_flex_image":
			err = cmdDownloadFlexImage(os.Args[1:])
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "list":
		err = cmdList(os.Args[2:])
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "inject":
		err = cmdInject(os.Args[2:])
	case "keys":
		err = cmdKeys(os.Args[2:])
	case "package_flex_image":
		err = cmdPackageFlexImage(os.Args[2:])
	case "download_flex_image":
		err = cmdDownloadFlexImage(os.Args[2:])
	case "parts":
		err = cmdParts(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("flexpack %s\n", version)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	manifestURL := fs.String("manifest", DefaultManifestURL, "recovery manifest URL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	images, err := FetchManifest(*manifestURL)
	if err != nil {
		return err
	}

	fmt.Printf("%d image(s) currently served:\n\n", len(images))
	for _, im := range images {
		fmt.Printf("  %s\n", im)
	}
	fmt.Printf("\nFetch one with:  flexpack fetch --channel %s\n", lowerChannel(images[0].Channel))
	return nil
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	channel := fs.String("channel", "stable", "channel to fetch (stable, lts, ltc, dev)")
	out := fs.String("o", "", "output path (default: the manifest's filename)")
	manifestURL := fs.String("manifest", DefaultManifestURL, "recovery manifest URL")
	noVerify := fs.Bool("no-verify", false, "skip sha1/md5/size verification")
	if err := fs.Parse(args); err != nil {
		return err
	}

	images, err := FetchManifest(*manifestURL)
	if err != nil {
		return err
	}

	image, err := SelectChannel(images, *channel)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "%s %s (%s)\n", image.Name, image.Version, image.Channel)
	fmt.Fprintf(os.Stderr, "  archive %s  ->  image %s\n",
		humanBytes(image.ZipFileSize), humanBytes(image.FileSize))

	return Fetch(image, *out, !*noVerify)
}

func lowerChannel(c string) string {
	b := []byte(c)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func cmdParts(args []string) error {
	fs := flag.NewFlagSet("parts", flag.ExitOnError)
	image := fs.String("image", "", "disk image to inspect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image == "" {
		return fmt.Errorf("--image is required")
	}

	f, err := os.Open(*image)
	if err != nil {
		return err
	}
	defer f.Close()

	parts, err := ReadGPT(f, 512)
	if err != nil {
		return err
	}
	for _, p := range parts {
		fmt.Println(p)
	}
	return nil
}

func cmdInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	image := fs.String("image", "", "image to modify (or to copy from, with -o)")
	out := fs.String("o", "", "write to a copy at this path instead of modifying in place")
	token := fs.String("token", "", "enrolment token, lowercase UUID")
	configFile := fs.String("config", "", "use this config.json instead of building one from --token")
	target := fs.String("path", flexConfigPath, "path inside the filesystem")
	partName := fs.String("partition", "STATE", "GPT partition name to write into")
	automate := fs.Bool("automate-setup", false, "add the OOBE setup-automation keys (see README caveat)")
	force := fs.Bool("force", false, "replace an existing config")
	dryRun := fs.Bool("dry-run", false, "report what would happen, change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image == "" {
		return fmt.Errorf("--image is required")
	}
	if (*token == "") == (*configFile == "") {
		return fmt.Errorf("give exactly one of --token or --config")
	}

	var data []byte
	var err error
	if *configFile != "" {
		data, err = LoadConfigFile(*configFile)
	} else {
		data, err = BuildConfig(*token, *automate, nil)
	}
	if err != nil {
		return err
	}

	work := *image
	if *out != "" && !*dryRun {
		fmt.Fprintf(os.Stderr, "  copying %s -> %s\n", *image, *out)
		if err := copyFile(*image, *out); err != nil {
			return err
		}
		work = *out
	}

	return Inject(InjectOptions{
		ImagePath: work,
		Target:    *target,
		Data:      data,
		Partition: *partName,
		Force:     *force,
		DryRun:    *dryRun,
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer o.Close()

	if _, err := io.Copy(o, in); err != nil {
		return err
	}
	return o.Sync()
}

func cmdKeys(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	all := fs.Bool("all", false, "include demo-mode, rollback and test-only keys")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println("OOBE configuration keys, from chrome/browser/ash/login/configuration_keys.cc")
	fmt.Println()
	fmt.Printf("  %-32s %-8s %-5s %s\n", "KEY", "TYPE", "SIDE", "STATUS")
	fmt.Printf("  %-32s %-8s %-5s %s\n", strings.Repeat("-", 32), "--------", "-----", "------")

	skip := map[string]bool{
		"enableDemoMode": true, "demoPreferencesNext": true, "networkOfflineDemo": true,
		"enrollmentRestoreAfterRollback": true, "testValue": true,
	}

	for _, k := range oobeKeys {
		if !*all && skip[k.Key] {
			continue
		}
		status := ""
		switch {
		case !k.Registered:
			status = "DROPPED (unregistered)"
		case k.EmittedByTool:
			status = "written by --automate_setup"
		case k.Side == sideDoc:
			status = "accepted, never delivered"
		}
		fmt.Printf("  %-32s %-8s %-5s %s\n", k.Key, k.Type, k.Side, status)
		fmt.Printf("      %s\n", k.Desc)
		if k.Warn != "" {
			fmt.Printf("      ! %s\n", k.Warn)
		}
	}
	if !*all {
		fmt.Println()
		fmt.Println("  (demo-mode, rollback and test keys hidden; pass --all to see them)")
	}
	return nil
}
