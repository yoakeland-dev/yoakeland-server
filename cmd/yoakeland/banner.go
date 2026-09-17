package main

import (
	"fmt"
	"math/rand"
	"os"
	"strings"

	"golang.org/x/term"
)

const bannerTitle = "✦ yoakeland ✦"

// bannerIndent is how far the title sits from the left edge — "slightly
// left aligned" rather than dead-centered.
const bannerIndent = 2

var starChars = []rune{'*', '.', '+', '·', '✦'}

// printBanner writes a small startup title to stdout, framed by a sparse
// starfield. Plain text, no color — this runs in a server's stdout log.
func printBanner() {
	width, isTTY := terminalWidth()
	if width <= 0 {
		width = 40
	}
	indent := bannerIndent
	if !isTTY {
		indent = 0
	}

	rng := rand.New(rand.NewSource(rand.Int63()))
	titleLine := strings.Repeat(" ", indent) + bannerTitle
	trailing := width - len([]rune(titleLine))

	fmt.Println()
	fmt.Println(starLine(rng, width))
	fmt.Println(titleLine + starLine(rng, trailing))
	fmt.Println(starLine(rng, width))
	fmt.Println()
}

// starLine renders n columns of mostly-blank space with the occasional
// star, used to frame the banner without drawing a hard box around it.
func starLine(rng *rand.Rand, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if rng.Float64() < 0.06 {
			b.WriteRune(starChars[rng.Intn(len(starChars))])
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// terminalWidth reports stdout's column width and whether it's actually a
// terminal (a pipe/log file gets a fixed-width starfield instead of one
// sized to a real window).
func terminalWidth() (width int, isTTY bool) {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return 0, false
	}
	w, _, err := term.GetSize(fd)
	if err != nil || w <= 0 {
		return 80, true
	}
	return w, true
}
