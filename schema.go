package main

import "fmt"

// The OOBE configuration schema, transcribed from Chromium's
// chrome/browser/ash/login/configuration_keys.{h,cc}.
//
// Two functions there decide what a config actually does:
//
//	ValidateConfiguration — walks kAllConfigurationKeys, type-checks any key
//	  it finds, and removes it from a working copy. Whatever is left over is
//	  logged as "Unknown configuration key". Note that leftovers do NOT make
//	  the config invalid; only a type mismatch sets valid = false.
//
//	FilterConfiguration — builds the dictionary handed to the JS or C++ side
//	  by iterating kAllConfigurationKeys. A key absent from that table is
//	  never copied across, so it reaches no handler at all.
//
// The practical consequence: an unregistered key is silently dropped. It does
// not fail, it does not warn the operator, it just does nothing. Hence the
// Registered field below — it is the difference between a key that works and
// a key that looks like it works.

type valueType int

const (
	typeString valueType = iota
	typeBool
)

func (v valueType) String() string {
	if v == typeBool {
		return "boolean"
	}
	return "string"
}

type handlerSide int

const (
	sideJS handlerSide = iota
	sideCPP
	sideBoth
	sideDoc
)

func (h handlerSide) String() string {
	switch h {
	case sideJS:
		return "JS"
	case sideCPP:
		return "C++"
	case sideBoth:
		return "both"
	default:
		return "doc"
	}
}

// KeySpec describes one OOBE configuration key.
type KeySpec struct {
	Key    string
	Type   valueType
	Side   handlerSide
	Screen string
	Desc   string

	// Registered reports whether the key appears in kAllConfigurationKeys.
	// False means Chromium declares and documents the key but never wired it
	// into the table, so FilterConfiguration drops it.
	Registered bool

	// Warn is a gotcha worth surfacing at the point of use.
	Warn string

	// EmittedByTool marks keys the real package_flex_image writes.
	EmittedByTool bool
}

// oobeKeys is the full schema. Order follows the OOBE flow, as in the source.
var oobeKeys = []KeySpec{
	// -- Welcome screen
	{Key: "welcomeNext", Type: typeBool, Side: sideJS, Screen: "Welcome", Registered: true, EmittedByTool: true,
		Desc: "Press Next on the welcome screen automatically."},
	{Key: "language", Type: typeString, Side: sideJS, Screen: "Welcome", Registered: true,
		Desc: "Preferred UI language."},
	{Key: "inputMethod", Type: typeString, Side: sideJS, Screen: "Welcome", Registered: true,
		Desc: "Preferred input method / keyboard layout."},
	{Key: "enableDemoMode", Type: typeBool, Side: sideBoth, Screen: "Welcome", Registered: true,
		Desc: "Run the demo mode setup flow.",
		Warn: "Demo mode, not enrolment. Almost certainly not what you want on managed hardware."},

	// -- Demo mode preferences
	{Key: "demoPreferencesNext", Type: typeBool, Side: sideJS, Screen: "Demo prefs", Registered: true,
		Desc: "Press Ok on the demo mode preferences screen automatically."},

	// -- Network screen
	{Key: "networkSelectGuid", Type: typeString, Side: sideJS, Screen: "Network", Registered: true,
		Desc: "Select the network with this GUID automatically."},
	{Key: "networkOfflineDemo", Type: typeBool, Side: sideJS, Screen: "Network", Registered: true,
		Desc: "Select the offline demo mode option automatically."},
	{Key: "networkUseConnected", Type: typeBool, Side: sideJS, Screen: "Network", Registered: true, EmittedByTool: true,
		Desc: "Use the first already-connected network instead of showing the picker."},
	{Key: "networkConfig", Type: typeString, Side: sideCPP, Screen: "Network", Registered: true,
		Desc: "Network configuration preserved across a rollback.",
		Warn: "Documented as a rollback artefact, not a provisioning mechanism. Do not assume it pre-seeds Wi-Fi on a fresh install."},

	// -- EULA screen
	{Key: "eulaSendStatistics", Type: typeBool, Side: sideJS, Screen: "EULA", Registered: true,
		Desc: "Set the usage-statistics opt-in that lives on the EULA screen."},
	{Key: "eulaAutoAccept", Type: typeBool, Side: sideJS, Screen: "EULA", Registered: true, EmittedByTool: true,
		Desc: "Accept the EULA without user interaction."},

	// -- ARC++ terms
	{Key: "arcTosAutoAccept", Type: typeBool, Side: sideBoth, Screen: "ARC++ ToS", Registered: true,
		Desc: "Accept the Play/ARC terms of service automatically.",
		Warn: "No effect on ChromeOS Flex — ARC is not supported there."},

	// -- Wizard controller
	{Key: "deviceRequisition", Type: typeString, Side: sideCPP, Screen: "Wizard", Registered: true,
		Desc: "Device requisition string, used for special device classes."},

	// -- Enrollment screen
	{Key: "enrollmentRestoreAfterRollback", Type: typeBool, Side: sideCPP, Screen: "Enrolment", Registered: true,
		Desc: "Marks that the device was enrolled before a rollback.",
		Warn: "The constant is kRestoreAfterRollback but the JSON key carries the enrollment prefix."},
	{Key: "enrollmentAssetId", Type: typeString, Side: sideCPP, Screen: "Enrolment", Registered: false,
		Desc: "Value to prefill into the Asset ID field on the Device Attributes step.",
		Warn: "NOT in kAllConfigurationKeys. FilterConfiguration drops it, so it reaches no handler and does nothing."},
	{Key: "enrollmentLocation", Type: typeString, Side: sideCPP, Screen: "Enrolment", Registered: true,
		Desc: "Value to prefill into the Location field on the Device Attributes step."},
	{Key: "enrollmentAutoAttributes", Type: typeBool, Side: sideCPP, Screen: "Enrolment", Registered: false,
		Desc: "Proceed through the device attributes step with preset values.",
		Warn: "NOT in kAllConfigurationKeys. FilterConfiguration drops it, so it reaches no handler and does nothing."},
	{Key: "enrollmentToken", Type: typeString, Side: sideCPP, Screen: "Enrolment", Registered: true, EmittedByTool: true,
		Desc: "Enrolment token. Currently used only for Flex auto-enrolment."},
	{Key: "skipEnrollmentSuccessScreen", Type: typeBool, Side: sideCPP, Screen: "Enrolment", Registered: true, EmittedByTool: true,
		Desc: "Proceed through the enrolment success screen automatically."},
	{Key: "skipUpdateOptOutScreen", Type: typeBool, Side: sideCPP, Screen: "Enrolment", Registered: true,
		Desc: "Skip the update opt-out screen."},
	{Key: "skipHidScreen", Type: typeBool, Side: sideCPP, Screen: "Enrolment", Registered: true,
		Desc: "Proceed through the HID (keyboard/mouse) detection screen automatically.",
		Warn: "The constant is kSkipHIDScreen but the JSON key is skipHidScreen — lowercase 'id'."},

	// -- Provenance
	{Key: "source", Type: typeString, Side: sideCPP, Screen: "Provenance", Registered: true, EmittedByTool: true,
		Desc: "What produced this config. Documented values: PACKAGING_TOOL, REMOTE_DEPLOYMENT."},

	// -- Documentation / test
	{Key: "desc", Type: typeString, Side: sideDoc, Screen: "Documentation", Registered: true,
		Desc: "Free-text note. Known to the validator so it raises no warning, but dropped before any handler sees it."},
	{Key: "testValue", Type: typeString, Side: sideBoth, Screen: "Test", Registered: true,
		Desc: "Test-only value."},
}

func lookupKey(name string) (KeySpec, bool) {
	for _, k := range oobeKeys {
		if k.Key == name {
			return k, true
		}
	}
	return KeySpec{}, false
}

// validSourceValues are the values Chromium documents for the source key.
var validSourceValues = []string{"PACKAGING_TOOL", "REMOTE_DEPLOYMENT"}

// -------------------------------------------------------------- validation

// Diagnostic is one finding about a config, mirroring what Chromium would log.
type Diagnostic struct {
	Level string // "error" | "warning" | "note"
	Key   string
	Msg   string
}

func (d Diagnostic) String() string {
	if d.Key == "" {
		return d.Level + ": " + d.Msg
	}
	return d.Level + ": " + d.Key + " — " + d.Msg
}

// ValidateConfig checks a decoded config the way Chromium's
// ValidateConfiguration and FilterConfiguration would, and reports what each
// key will actually do on the device.
//
// fatal is true when a type mismatch would make Chromium reject the config.
func ValidateConfig(cfg map[string]any) (diags []Diagnostic, fatal bool) {
	for name, val := range cfg {
		spec, known := lookupKey(name)
		if !known {
			diags = append(diags, Diagnostic{"warning", name,
				"not a known OOBE configuration key; Chromium logs it as unknown and drops it"})
			continue
		}

		switch spec.Type {
		case typeBool:
			if _, ok := val.(bool); !ok {
				diags = append(diags, Diagnostic{"error", name,
					"must be a boolean; a type mismatch makes ValidateConfiguration return false"})
				fatal = true
			}
		case typeString:
			if _, ok := val.(string); !ok {
				diags = append(diags, Diagnostic{"error", name,
					"must be a string; a type mismatch makes ValidateConfiguration return false"})
				fatal = true
			}
		}

		if !spec.Registered {
			diags = append(diags, Diagnostic{"warning", name,
				"declared in Chromium but absent from kAllConfigurationKeys, so it is filtered out and has no effect"})
		}
		if spec.Warn != "" {
			diags = append(diags, Diagnostic{"note", name, spec.Warn})
		}
		if name == "source" {
			if s, ok := val.(string); ok {
				valid := false
				for _, v := range validSourceValues {
					if s == v {
						valid = true
					}
				}
				if !valid {
					diags = append(diags, Diagnostic{"note", name,
						"value is outside the documented set (PACKAGING_TOOL, REMOTE_DEPLOYMENT)"})
				}
			}
		}
	}
	return diags, fatal
}

// CoerceValue turns a --set value into the type the schema demands.
func CoerceValue(spec KeySpec, raw string) (any, error) {
	switch spec.Type {
	case typeBool:
		switch raw {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		default:
			return nil, fmt.Errorf("%s expects a boolean (true/false), got %q", spec.Key, raw)
		}
	default:
		return raw, nil
	}
}
