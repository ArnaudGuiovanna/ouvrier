package ide

import "image/color"

// Catppuccin Macchiato palette used by the IDE.
const (
	baseHex     = "#24273a"
	mantleHex   = "#1e2030"
	surface0Hex = "#363a4f"
	surface1Hex = "#494d64"
	surface2Hex = "#5b6078"
	textHex     = "#cad3f5"
	subtext0Hex = "#a5adcb"
	overlay1Hex = "#8087a2"
	overlay2Hex = "#939ab7"
	accentHex   = "#c6a0f6"
	linkHex     = "#8aadf4"
	okHex       = "#a6da95"
	runningHex  = "#7dc4e4"
	failHex     = "#ed8796"
	gateHex     = "#eed49f"

	diagErrorHex = "#ed8796"
	diagWarnHex  = "#eed49f"
	diagInfoHex  = "#8aadf4"
	diagHintHex  = "#8bd5ca"
)

// baseRGBA is the background color used for the IDE window.
var baseRGBA = color.RGBA{R: 0x24, G: 0x27, B: 0x3a, A: 0xff}
