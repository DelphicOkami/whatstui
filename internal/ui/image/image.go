// Package image renders images inline in the TUI using the kitty graphics
// protocol with Unicode placeholders.
//
// The protocol works in two halves:
//
//  1. Transmission. The image bytes are sent to the terminal once, in an
//     APC escape sequence (`ESC _ G ... ; <base64> ESC \`). With U=1, the
//     terminal stores the image keyed by `i=<id>` but doesn't display it.
//
//  2. Placement. Each cell that should show a piece of the image prints a
//     U+10EEEE character with two combining diacritics (row, column) and an
//     SGR foreground colour whose value encodes the image ID. Bubble Tea /
//     lipgloss can render these strings normally — they're just text — and
//     kitty draws the right tile of the image on top of each cell.
//
// This split is what makes the protocol Bubble Tea-friendly: the heavy
// payload goes out-of-band (no width/colour bookkeeping for lipgloss), and
// the in-band string is plain UTF-8.
//
// Detection uses environment variables only — we don't probe the terminal,
// so a startup with $TERM=xterm-kitty inside tmux still claims support;
// callers should trust the env or expose an opt-out flag.
package image

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	stdimage "image"
	"image/png"
	"io"
	"os"
	"strings"

	// Side-effect imports register decoders for the formats whatsmeow
	// commonly hands us. WebP isn't in stdlib; messages with that mime fall
	// back to "[image]" until we pull in golang.org/x/image/webp.
	_ "image/gif"
	_ "image/jpeg"
)

// ErrUnsupported is returned by Render when the current terminal doesn't
// speak the kitty graphics protocol. Callers should fall back to a text
// placeholder (e.g. "[image]").
var ErrUnsupported = errors.New("kitty graphics protocol not supported by this terminal")

// Supported reports whether the running terminal speaks the kitty graphics
// protocol. We detect via env vars only:
//   - $TERM contains "kitty" (kitty itself sets xterm-kitty)
//   - $KITTY_WINDOW_ID set (kitty)
//   - $TERM_PROGRAM == "ghostty" or $GHOSTTY_RESOURCES_DIR set
//   - $TERM_PROGRAM == "WezTerm" or $WEZTERM_PANE set
//
// Inside tmux/screen this can produce a false positive — graphics may be
// stripped before reaching the outer terminal. Users who hit that should
// set CHARMING_WHATSMEOW_NO_KITTY=1 to force the fallback.
func Supported() bool {
	if os.Getenv("CHARMING_WHATSMEOW_NO_KITTY") != "" {
		return false
	}
	if strings.Contains(os.Getenv("TERM"), "kitty") {
		return true
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return true
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "ghostty", "WezTerm":
		return true
	}
	if os.Getenv("GHOSTTY_RESOURCES_DIR") != "" || os.Getenv("WEZTERM_PANE") != "" {
		return true
	}
	// Inside tmux the outer terminal isn't directly visible from env.
	// Trust $TMUX as an opt-in: if the user is running us under tmux,
	// they almost certainly have allow-passthrough configured (otherwise
	// nothing — icat included — would render). Transmit handles the
	// passthrough wrapping. Set CHARMING_WHATSMEOW_NO_KITTY=1 to opt out.
	if os.Getenv("TMUX") != "" {
		return true
	}
	return false
}

// inTmux reports whether we're running under a tmux session. Used by
// Transmit to wrap APC chunks in tmux's passthrough DCS so they reach
// the outer terminal.
func inTmux() bool {
	return os.Getenv("TMUX") != ""
}

// chunkSize is kitty's recommended max payload bytes per APC chunk (4096
// base64 chars; the spec says <=4096 to be safe across terminal buffers).
const chunkSize = 4096

// Transmit writes the kitty graphics transmission for path to w, tagged
// with image ID. Subsequent placeholder strings referencing the same id
// will display this image. The payload is silenced (q=2) and uses virtual
// placement (U=1) so the cursor doesn't move and no visible glyph is
// emitted at the current cursor location.
//
// rows and cols set the virtual placement size in cells (kitty's r=/c=
// metadata). They MUST match the grid the caller will later render with
// Placeholder, otherwise kitty either crops (placeholder smaller than the
// virtual placement) or repeats edge cells (placeholder larger). Pass 0
// for either to let kitty derive size from native pixel dimensions, but
// this almost always produces truncation in TUIs and should be avoided.
//
// path must point to a PNG, JPEG, or GIF; other formats return an error.
func Transmit(w io.Writer, id uint32, rows, cols int, path string) error {
	pngBytes, err := loadAsPNG(path)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(pngBytes)

	// First chunk carries the metadata. Subsequent chunks only carry m=
	// (more) and the next slice. Final chunk has m=0.
	for i := 0; i < len(encoded); i += chunkSize {
		end := i + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		more := 0
		if end < len(encoded) {
			more = 1
		}
		var header string
		if i == 0 {
			header = fmt.Sprintf("f=100,a=T,U=1,i=%d,r=%d,c=%d,q=2,m=%d", id, rows, cols, more)
		} else {
			header = fmt.Sprintf("m=%d", more)
		}
		chunk := fmt.Sprintf("\x1b_G%s;%s\x1b\\", header, encoded[i:end])
		if inTmux() {
			// tmux passthrough: wrap in DCS (ESC P tmux; ... ESC \) and
			// double every internal ESC. tmux strips one layer of ESC
			// doubling and re-emits the inner sequence to the outer
			// terminal. Each chunk is wrapped independently — tmux has
			// an internal buffer limit on DCS payload length and a single
			// wrapped multi-chunk transmission can exceed it.
			chunk = "\x1bPtmux;" + strings.ReplaceAll(chunk, "\x1b", "\x1b\x1b") + "\x1b\\"
		}
		if _, err := io.WriteString(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Placeholder returns a multi-line string that occupies rows × cols cells
// and references image id. Each line ends with a newline except the last,
// so callers can drop the result straight into a lipgloss block.
//
// rows and cols are 1-based counts. The function clamps both to the size
// of the diacritic table; in practice this is plenty (297 in each axis).
func Placeholder(id uint32, rows, cols int) string {
	if rows <= 0 || cols <= 0 {
		return ""
	}
	if rows > len(rowColumnDiacritics) {
		rows = len(rowColumnDiacritics)
	}
	if cols > len(rowColumnDiacritics) {
		cols = len(rowColumnDiacritics)
	}

	// Image ID is encoded as the SGR 256-colour foreground when id <= 255,
	// or as a 24-bit truecolour when larger. Kitty reads R/G/B as the
	// three low bytes of the ID. We restrict to <= 0xFFFFFF (3 bytes) and
	// always use truecolour for consistency.
	r := byte((id >> 16) & 0xFF)
	g := byte((id >> 8) & 0xFF)
	b := byte(id & 0xFF)

	var out strings.Builder
	for row := 0; row < rows; row++ {
		if row > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm", r, g, b)
		for col := 0; col < cols; col++ {
			out.WriteRune(0x10EEEE)
			out.WriteRune(rowColumnDiacritics[row])
			out.WriteRune(rowColumnDiacritics[col])
		}
		out.WriteString("\x1b[39m")
	}
	return out.String()
}

// FitCells picks placeholder rows × cols for an image of pixWidth × pixHeight,
// clamped so the result never exceeds maxRows / maxCols. Aspect ratio is
// preserved assuming a 2:1 cell aspect (height:width pixels) — close enough
// for typical monospace fonts and avoids the round-trip of querying real
// cell pixel size from the terminal.
func FitCells(pixWidth, pixHeight, maxRows, maxCols int) (rows, cols int) {
	if pixWidth <= 0 || pixHeight <= 0 || maxRows <= 0 || maxCols <= 0 {
		return 0, 0
	}
	const cellAspect = 2.0 // each cell ≈ twice as tall as wide in pixels
	imgAspect := float64(pixWidth) / float64(pixHeight)
	cellWHRatio := imgAspect / cellAspect

	// Try filling maxCols first.
	cols = maxCols
	rows = int(float64(cols)/cellWHRatio + 0.5)
	if rows > maxRows {
		rows = maxRows
		cols = int(float64(rows)*cellWHRatio + 0.5)
	}
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	return rows, cols
}

// loadAsPNG reads path and returns the bytes as PNG. Already-PNG files are
// returned unchanged; other formats are decoded and re-encoded so the
// kitty payload is always f=100 (PNG).
func loadAsPNG(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Sniff the first 8 bytes for the PNG signature so we skip an
	// unnecessary decode/encode round-trip on already-PNG files.
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	if n == 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		rest, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return append(head[:n], rest...), nil
	}

	// Not PNG — rewind, decode, re-encode.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := stdimage.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}
