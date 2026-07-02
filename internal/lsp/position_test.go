package lsp

import "testing"

func TestRuneColumnUTF8(t *testing.T) {
	// line "é= x": é=2 bytes, '='=1, ' '=1 → 'x' starts at byte offset 4
	got := RuneColumn("é= x", 4, EncodingUTF8)
	if got != 3 {
		t.Errorf("UTF-8 x at byte 4: want rune 3, got %d", got)
	}
}

func TestRuneColumnUTF16(t *testing.T) {
	// line "é= x": é=1 UTF-16 unit, '='=1, ' '=1 → 'x' at unit offset 3
	got := RuneColumn("é= x", 3, EncodingUTF16)
	if got != 3 {
		t.Errorf("UTF-16 x at unit 3: want rune 3, got %d", got)
	}
}

func TestRuneColumnClamp(t *testing.T) {
	// Clamp beyond end
	got := RuneColumn("abc", 99, EncodingUTF16)
	if got != 3 {
		t.Errorf("clamp UTF-16: want 3, got %d", got)
	}
	// Zero character
	got = RuneColumn("abc", 0, EncodingUTF8)
	if got != 0 {
		t.Errorf("zero UTF-8: want 0, got %d", got)
	}
}

func TestRuneColumnSupplementary(t *testing.T) {
	// "😀x": 😀 is 2 UTF-16 units, 4 UTF-8 bytes; x is rune index 1
	got := RuneColumn("😀x", 2, EncodingUTF16)
	if got != 1 {
		t.Errorf("UTF-16 emoji: want rune 1, got %d", got)
	}
	got = RuneColumn("😀x", 4, EncodingUTF8)
	if got != 1 {
		t.Errorf("UTF-8 emoji: want rune 1, got %d", got)
	}
}

func TestEncodedColumnUTF8(t *testing.T) {
	// "é= x": é=2 bytes, '='=1, ' '=1, 'x'=1 → rune 3 is 'x' at byte offset 4
	got := EncodedColumn("é= x", 3, EncodingUTF8)
	if got != 4 {
		t.Errorf("UTF-8 rune 3 → want byte 4, got %d", got)
	}
}

func TestEncodedColumnUTF16(t *testing.T) {
	// "é= x": é=1 UTF-16 unit, '='=1, ' '=1 → rune 3 is 'x' at unit offset 3
	got := EncodedColumn("é= x", 3, EncodingUTF16)
	if got != 3 {
		t.Errorf("UTF-16 rune 3 → want unit 3, got %d", got)
	}
}

func TestEncodedColumnRoundTrip(t *testing.T) {
	cases := []struct {
		line    string
		runeCol int
		enc     PositionEncoding
	}{
		{"é= x", 0, EncodingUTF8},
		{"é= x", 1, EncodingUTF8},
		{"é= x", 3, EncodingUTF8},
		{"é= x", 0, EncodingUTF16},
		{"é= x", 2, EncodingUTF16},
		{"é= x", 3, EncodingUTF16},
		{"😀x", 0, EncodingUTF8},
		{"😀x", 1, EncodingUTF8},
		{"😀x", 0, EncodingUTF16},
		{"😀x", 1, EncodingUTF16},
	}
	for _, c := range cases {
		enc := EncodedColumn(c.line, c.runeCol, c.enc)
		got := RuneColumn(c.line, enc, c.enc)
		if got != c.runeCol {
			t.Errorf("round-trip %q runeCol=%d enc=%s: EncodedColumn→%d, RuneColumn→%d",
				c.line, c.runeCol, c.enc, enc, got)
		}
	}
}

func TestLineAt(t *testing.T) {
	doc := "line0\nline1\nline2"
	cases := []struct {
		row  int
		want string
	}{
		{0, "line0"},
		{1, "line1"},
		{2, "line2"},
		{3, ""},
		{-1, ""},
	}
	for _, c := range cases {
		got := LineAt(doc, c.row)
		if got != c.want {
			t.Errorf("LineAt(doc, %d) = %q, want %q", c.row, got, c.want)
		}
	}
}
