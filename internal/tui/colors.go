package tui

import "image/color"

const (
	blackHex    = "#0a0a0a"
	offWhiteHex = "#fafafa"
	greenHex    = "#00d27a"
)

var (
	backgroundColor = color.RGBA{R: 0x0a, G: 0x0a, B: 0x0a, A: 0xff}
	foregroundColor = color.RGBA{R: 0xfa, G: 0xfa, B: 0xfa, A: 0xff}
)
