package tui

import "image/color"

const (
	// --- legacy hue names, repointed to Catppuccin Macchiato (kept so existing call sites compile) ---
	blackHex    = "#24273a" // was app-black → now base bg
	offWhiteHex = "#cad3f5" // text
	greenHex    = "#a6da95" // SUCCESS green (ok)
	mutedHex    = "#939ab7" // overlay2 (dim)
	cyanHex     = "#7dc4e4" // sapphire (live/running)
	yellowHex   = "#eed49f" // yellow (attention/gate)
	redHex      = "#ed8796" // red (fail)
	dimGreenHex = "#494d64" // surface1 (rules/borders)

	// --- Catppuccin Macchiato surfaces ---
	bgCrustHex  = "#181926"
	bgMantleHex = "#1e2030"
	bgBaseHex   = "#24273a"
	surface0Hex = "#363a4f"
	surface1Hex = "#494d64"
	surface2Hex = "#5b6078"

	// --- text tiers ---
	textHex     = "#cad3f5"
	subtext1Hex = "#b8c0e0"
	subtext0Hex = "#a5adcb"
	overlay2Hex = "#939ab7"
	overlay1Hex = "#8087a2"
	overlay0Hex = "#6e738d" // rules/separators ONLY (too low-contrast for text)

	// --- semantic ---
	accentHex    = "#c6a0f6" // mauve — focus / identity / Ouvrier-ness ONLY
	accentDimHex = "#494d64"
	linkHex      = "#8aadf4" // blue — keybind hints / links
	okHex        = "#a6da95" // green — success ONLY
	runningHex   = "#7dc4e4" // sapphire — running / streaming
	failHex      = "#ed8796" // red
	gateHex      = "#eed49f" // yellow — approval gate / attention
	magentaHex   = "#c6a0f6" // = accent (kept name for existing sites)

	// --- LSP diagnostic severities ---
	diagErrorHex = "#ed8796"
	diagWarnHex  = "#eed49f"
	diagInfoHex  = "#8aadf4"
	diagHintHex  = "#8bd5ca"

	// --- diff ---
	diffAddHex = "#a6da95"
	diffDelHex = "#ed8796"
)

var (
	backgroundColor = color.RGBA{R: 0x24, G: 0x27, B: 0x3a, A: 0xff}
	foregroundColor = color.RGBA{R: 0xca, G: 0xd3, B: 0xf5, A: 0xff}
)
