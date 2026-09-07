package image

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSupportedEnvDetection(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"none", nil, false},
		{"kitty TERM", map[string]string{"TERM": "xterm-kitty"}, true},
		{"kitty window id", map[string]string{"KITTY_WINDOW_ID": "1"}, true},
		{"ghostty program", map[string]string{"TERM_PROGRAM": "ghostty"}, true},
		{"ghostty resources", map[string]string{"GHOSTTY_RESOURCES_DIR": "/x"}, true},
		{"wezterm program", map[string]string{"TERM_PROGRAM": "WezTerm"}, true},
		{"wezterm pane", map[string]string{"WEZTERM_PANE": "0"}, true},
		{"opt-out wins", map[string]string{
			"TERM":                          "xterm-kitty",
			"CHARMING_WHATSMEOW_NO_KITTY":   "1",
		}, false},
		{"alacritty (unsupported)", map[string]string{"TERM": "alacritty"}, false},
		{"tmux opt-in", map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv unsets at test end; clear all relevant first so
			// previous-case env doesn't leak into "none".
			for _, k := range []string{
				"TERM", "KITTY_WINDOW_ID", "TERM_PROGRAM",
				"GHOSTTY_RESOURCES_DIR", "WEZTERM_PANE",
				"CHARMING_WHATSMEOW_NO_KITTY", "TMUX",
			} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := Supported(); got != tc.want {
				t.Fatalf("Supported() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlaceholderShape(t *testing.T) {
	got := Placeholder(42, 3, 5)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), got)
	}
	for i, line := range lines {
		// Each line: SGR-set, then 5 placeholder triplets (U+10EEEE + 2 diacritics), then SGR-reset.
		if !strings.HasPrefix(line, "\x1b[38;2;0;0;42m") {
			t.Errorf("line %d: missing fg-id SGR prefix: %q", i, line)
		}
		if !strings.HasSuffix(line, "\x1b[39m") {
			t.Errorf("line %d: missing SGR reset suffix: %q", i, line)
		}
		// Count U+10EEEE — should be exactly cols (5).
		count := strings.Count(line, string(rune(0x10EEEE)))
		if count != 5 {
			t.Errorf("line %d: expected 5 placeholder runes, got %d", i, count)
		}
	}
}

func TestPlaceholderEncodesRowColumn(t *testing.T) {
	got := Placeholder(1, 2, 2)
	// row 0 col 0: diacritics[0] twice; row 1 col 1: diacritics[1] twice.
	if !strings.ContainsRune(got, rowColumnDiacritics[0]) {
		t.Errorf("output missing row/col 0 diacritic")
	}
	if !strings.ContainsRune(got, rowColumnDiacritics[1]) {
		t.Errorf("output missing row/col 1 diacritic")
	}
	// Sanity: every rune in the output is valid UTF-8 (no broken sequences).
	if !utf8.ValidString(got) {
		t.Errorf("placeholder is not valid UTF-8")
	}
}

func TestPlaceholderClampsToTableSize(t *testing.T) {
	// Should not panic when asked for more rows than diacritics exist.
	got := Placeholder(1, len(rowColumnDiacritics)+10, 1)
	lines := strings.Split(got, "\n")
	if len(lines) != len(rowColumnDiacritics) {
		t.Fatalf("expected clamp to %d lines, got %d", len(rowColumnDiacritics), len(lines))
	}
}

func TestFitCells(t *testing.T) {
	cases := []struct {
		w, h, maxR, maxC int
		wantR, wantC     int
	}{
		// Square image, 2:1 cell aspect → cols/rows = 0.5; pick rows=10, cols=5.
		{100, 100, 10, 30, 10, 5},
		// Wide image (4:1) → cols/rows = 2; with maxCols=30, rows=15. Capped at maxR=10 → cols=20.
		{400, 100, 10, 30, 10, 20},
		// Tall image (1:4) → cols/rows = 0.125; maxCols=30, rows=240. Capped at 10 → cols=1.
		{100, 400, 10, 30, 10, 1},
		// Degenerate inputs → zero.
		{0, 100, 10, 10, 0, 0},
	}
	for _, tc := range cases {
		gotR, gotC := FitCells(tc.w, tc.h, tc.maxR, tc.maxC)
		if gotR != tc.wantR || gotC != tc.wantC {
			t.Errorf("FitCells(%d,%d,%d,%d) = (%d,%d), want (%d,%d)",
				tc.w, tc.h, tc.maxR, tc.maxC, gotR, gotC, tc.wantR, tc.wantC)
		}
	}
}

func TestTransmitChunksAndPNGPassthrough(t *testing.T) {
	// Disable tmux passthrough wrapping so the assertions below can match
	// the bare APC envelope. The wrapping path is exercised separately.
	t.Setenv("TMUX", "")

	// Build an in-memory PNG (already-PNG path skips the decode/re-encode).
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.png")
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: byte(x * 64), A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pngBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Transmit(&out, 7, 8, 20, path); err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "\x1b_G") {
		t.Errorf("expected APC start, got %q", got[:min(20, len(got))])
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Errorf("expected APC terminator at end")
	}
	if !strings.Contains(got, "i=7") {
		t.Errorf("expected i=7 in metadata")
	}
	if !strings.Contains(got, "U=1") {
		t.Errorf("expected U=1 (unicode placement) in metadata")
	}
	if !strings.Contains(got, "r=8") || !strings.Contains(got, "c=20") {
		t.Errorf("expected r=8 and c=20 in metadata, got %q", got[:min(80, len(got))])
	}
}

func TestTransmitTmuxPassthrough(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")

	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.png")
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pngBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Transmit(&out, 1, 1, 1, path); err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	got := out.String()
	// Each chunk wrapped in DCS: ESC P tmux ; ... ESC \
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Errorf("expected tmux DCS prefix, got %q", got[:min(20, len(got))])
	}
	// Inner ESCs are doubled: the original APC prefix \x1b_G becomes \x1b\x1b_G.
	if !strings.Contains(got, "\x1b\x1b_G") {
		t.Errorf("expected doubled ESC before _G inside tmux wrap")
	}
}
