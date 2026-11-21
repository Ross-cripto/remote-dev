package panel

import (
	"regexp"
	"strings"
	"unicode"
)

const terminalTabWidth = 4

var terminalEscapes = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|[\\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\\\)|[ -/]*[0-~])")

var wideRunes = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1100, Hi: 0x115f, Stride: 1},
		{Lo: 0x2329, Hi: 0x232a, Stride: 1},
		{Lo: 0x2e80, Hi: 0xa4cf, Stride: 1},
		{Lo: 0xac00, Hi: 0xd7a3, Stride: 1},
		{Lo: 0xf900, Hi: 0xfaff, Stride: 1},
		{Lo: 0xfe10, Hi: 0xfe19, Stride: 1},
		{Lo: 0xfe30, Hi: 0xfe6f, Stride: 1},
		{Lo: 0xff01, Hi: 0xff60, Stride: 1},
		{Lo: 0xffe0, Hi: 0xffe6, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1f300, Hi: 0x1f64f, Stride: 1},
		{Lo: 0x1f680, Hi: 0x1f6ff, Stride: 1},
		{Lo: 0x1f900, Hi: 0x1f9ff, Stride: 1},
		{Lo: 0x1fa70, Hi: 0x1faff, Stride: 1},
		{Lo: 0x20000, Hi: 0x3fffd, Stride: 1},
	},
}

func stripEscapes(value string) string {
	return terminalEscapes.ReplaceAllString(value, "")
}

func runeWidth(character rune) int {
	if unicode.IsOneOf([]*unicode.RangeTable{unicode.Mn, unicode.Me}, character) {
		return 0
	}
	if unicode.Is(wideRunes, character) {
		return 2
	}
	return 1
}

func cellWidth(value string) int {
	width := 0
	for _, character := range value {
		width += runeWidth(character)
	}
	return width
}

func wrapCells(value string, width int) []string {
	if width < 1 {
		return []string{""}
	}
	var lines []string
	var line strings.Builder
	column := 0
	write := func(character rune, cells int) {
		if column+cells > width {
			lines = append(lines, line.String())
			line.Reset()
			column = 0
		}
		if cells > 0 || column > 0 {
			line.WriteRune(character)
			column += cells
		}
	}
	for _, character := range value {
		if character == '\t' {
			spaces := terminalTabWidth - column%terminalTabWidth
			for range spaces {
				write(' ', 1)
			}
		} else if unicode.IsPrint(character) {
			cells := runeWidth(character)
			if cells > width {
				character, cells = '?', 1
			}
			write(character, cells)
		}
	}
	return append(lines, line.String())
}

func outputLines(output string, width int) []string {
	output = strings.Trim(stripEscapes(output), "\r\n")
	if output == "" {
		return nil
	}
	var lines []string
	for line := range strings.SplitSeq(output, "\n") {
		lines = append(lines, wrapCells(line, width)...)
	}
	return lines
}
