package dundcli

import (
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// qrText is s as a QR code for a terminal: two modules per character cell
// (half blocks), with the 4-module quiet zone. Each line sets black on
// white itself, so the code reads the same on a dark or a light terminal
// theme (a phone camera wants dark modules on light).
func qrText(s string) (string, error) {
	q, err := qrcode.New(s, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bits := q.Bitmap() // true: dark
	var b strings.Builder
	for y := 0; y < len(bits); y += 2 {
		b.WriteString("\x1b[30;47m")
		for x := range bits[y] {
			top, bot := bits[y][x], y+1 < len(bits) && bits[y+1][x]
			switch {
			case top && bot:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bot:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}
