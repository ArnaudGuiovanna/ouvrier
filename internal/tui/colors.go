package tui

import "image/color"

const (
	blackHex    = "#0a0a0a"
	offWhiteHex = "#fafafa"
	greenHex    = "#00d27a"
	mutedHex    = "#7a8290"
	cyanHex     = "#56b6ff"
	yellowHex   = "#e5c07b"
	redHex      = "#ff5f5f"
	dimGreenHex = "#1f3d31"
	magentaHex  = "#c678dd"
)

var (
	backgroundColor = color.RGBA{R: 0x0a, G: 0x0a, B: 0x0a, A: 0xff}
	foregroundColor = color.RGBA{R: 0xfa, G: 0xfa, B: 0xfa, A: 0xff}
)
