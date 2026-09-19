// Command ansi2svg renders a terminal capture (`tmux capture-pane -e -p`)
// as an SVG screenshot for the documentation: real colours, crisp box
// drawing, a window frame. Used by scripts/screenshots.sh.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	cw       = 8.43 // cell width at 14px monospace
	lh       = 18.0 // line height
	pad      = 18.0
	titleBar = 34.0
	defFG    = "#d5dae3"
	defBG    = "#10141b"
)

type style struct {
	fg, bg          string
	bold, underline bool
	dim, reverse    bool
}

type cell struct {
	r rune
	s style
}

var base16 = []string{
	"#1b1f27", "#f7768e", "#a6e3a1", "#f9e2af", "#89b4fa", "#cba6f7", "#89dceb", "#bac2de",
	"#6c7086", "#ff8fa3", "#b9f2b4", "#fff0b8", "#a4c8ff", "#dcbcff", "#a6ecf5", "#e6e9f0",
}

func xterm(n int) string {
	switch {
	case n < 16:
		return base16[n]
	case n < 232:
		n -= 16
		lv := []int{0, 95, 135, 175, 215, 255}
		return fmt.Sprintf("#%02x%02x%02x", lv[n/36], lv[(n/6)%6], lv[n%6])
	default:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}

func sgr(st *style, params string) {
	if params == "" {
		params = "0"
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		n, _ := strconv.Atoi(ps[i])
		switch {
		case n == 0:
			*st = style{}
		case n == 1:
			st.bold = true
		case n == 2:
			st.dim = true
		case n == 22:
			st.bold, st.dim = false, false
		case n == 4:
			st.underline = true
		case n == 24:
			st.underline = false
		case n == 7:
			st.reverse = true
		case n == 27:
			st.reverse = false
		case n >= 30 && n <= 37:
			st.fg = base16[n-30]
		case n >= 90 && n <= 97:
			st.fg = base16[n-90+8]
		case n == 39:
			st.fg = ""
		case n >= 40 && n <= 47:
			st.bg = base16[n-40]
		case n >= 100 && n <= 107:
			st.bg = base16[n-100+8]
		case n == 49:
			st.bg = ""
		case (n == 38 || n == 48) && i+1 < len(ps):
			var col string
			if ps[i+1] == "5" && i+2 < len(ps) {
				k, _ := strconv.Atoi(ps[i+2])
				col = xterm(k)
				i += 2
			} else if ps[i+1] == "2" && i+4 < len(ps) {
				r, _ := strconv.Atoi(ps[i+2])
				g, _ := strconv.Atoi(ps[i+3])
				b, _ := strconv.Atoi(ps[i+4])
				col = fmt.Sprintf("#%02x%02x%02x", r, g, b)
				i += 4
			}
			if n == 38 {
				st.fg = col
			} else {
				st.bg = col
			}
		}
	}
}

func parse(r io.Reader) [][]cell {
	var rows [][]cell
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var st style
	for sc.Scan() {
		line := sc.Text()
		var row []cell
		for i := 0; i < len(line); {
			if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
				j := i + 2
				for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
					j++
				}
				if j < len(line) && line[j] == 'm' {
					sgr(&st, line[i+2:j])
				}
				i = j + 1
				continue
			}
			ru, size := utf8.DecodeRuneInString(line[i:])
			i += size
			if ru == '\t' {
				for k := 0; k < 4; k++ {
					row = append(row, cell{' ', st})
				}
				continue
			}
			row = append(row, cell{ru, st})
		}
		rows = append(rows, row)
	}
	// Trim trailing blank cells and rows.
	for i := range rows {
		n := len(rows[i])
		for n > 0 && rows[i][n-1].r == ' ' && rows[i][n-1].s.bg == "" && !rows[i][n-1].s.reverse {
			n--
		}
		rows[i] = rows[i][:n]
	}
	for len(rows) > 0 && len(rows[len(rows)-1]) == 0 {
		rows = rows[:len(rows)-1]
	}
	return rows
}

func colors(s style) (fg, bg string) {
	fg, bg = s.fg, s.bg
	if fg == "" {
		fg = defFG
	}
	if s.reverse {
		if bg == "" {
			bg = defBG
		}
		fg, bg = bg, fg
	}
	if s.dim && !s.reverse && s.fg == "" {
		fg = "#7f8796"
	}
	return
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// boxPath draws rounded-border glyphs as vectors so borders join cleanly.
func boxPath(r rune, x, y float64) string {
	cx, cy := x+cw/2, y+lh/2
	switch r {
	case '─':
		return fmt.Sprintf("M%.2f %.2fH%.2f", x, cy, x+cw)
	case '│':
		return fmt.Sprintf("M%.2f %.2fV%.2f", cx, y, y+lh)
	case '╭':
		return fmt.Sprintf("M%.2f %.2fQ%.2f %.2f %.2f %.2f", x+cw, cy, cx, cy, cx, y+lh)
	case '╮':
		return fmt.Sprintf("M%.2f %.2fQ%.2f %.2f %.2f %.2f", x, cy, cx, cy, cx, y+lh)
	case '╰':
		return fmt.Sprintf("M%.2f %.2fQ%.2f %.2f %.2f %.2f", cx, y, cx, cy, x+cw, cy)
	case '╯':
		return fmt.Sprintf("M%.2f %.2fQ%.2f %.2f %.2f %.2f", cx, y, cx, cy, x, cy)
	}
	return ""
}

func main() {
	title := flag.String("title", "spoor", "window title")
	cols := flag.Int("cols", 0, "canvas width in cells (default: widest line)")
	flag.Parse()
	rows := parse(os.Stdin)
	width := *cols
	for _, r := range rows {
		width = max(width, len(r))
	}
	W := pad*2 + float64(width)*cw
	H := titleBar + pad + float64(len(rows))*lh + pad
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', 'Liberation Mono', monospace" font-size="14">`+"\n", W, H, W, H)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" rx="10" fill="%s"/>`+"\n", W, H, defBG)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" rx="10" fill="#1a1f29"/><rect y="%.0f" width="%.0f" height="10" fill="#1a1f29"/>`+"\n", W, titleBar, titleBar-10, W)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="6" fill="%s"/>`, 20+float64(i)*20, titleBar/2, c)
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="#8b93a3" text-anchor="middle" font-size="13">%s</text>`+"\n", W/2, titleBar/2+4.5, esc(*title))
	top := titleBar + pad/2
	var paths = map[string]*strings.Builder{}
	for ri, row := range rows {
		y := top + float64(ri)*lh
		// backgrounds
		for i := 0; i < len(row); {
			_, bg := colors(row[i].s)
			j := i + 1
			for j < len(row) {
				if _, bg2 := colors(row[j].s); bg2 != bg {
					break
				}
				j++
			}
			if bg != "" && bg != defBG {
				fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.0f" fill="%s"/>`, pad+float64(i)*cw, y, float64(j-i)*cw, lh, bg)
			}
			i = j
		}
		// text runs: contiguous ASCII with one style; other runes one cell each
		for i := 0; i < len(row); {
			c := row[i]
			x := pad + float64(i)*cw
			fg, _ := colors(c.s)
			if p := boxPath(c.r, x, y); p != "" {
				pb := paths[fg]
				if pb == nil {
					pb = &strings.Builder{}
					paths[fg] = pb
				}
				pb.WriteString(p)
				i++
				continue
			}
			j := i + 1
			if c.r < 0x80 {
				for j < len(row) && row[j].r < 0x80 && row[j].s == c.s {
					j++
				}
			}
			text := make([]rune, 0, j-i)
			for _, cc := range row[i:j] {
				text = append(text, cc.r)
			}
			if strings.TrimSpace(string(text)) != "" {
				attrs := fmt.Sprintf(`fill="%s"`, fg)
				if c.s.bold {
					attrs += ` font-weight="700"`
				}
				if c.s.underline {
					attrs += ` text-decoration="underline"`
				}
				if c.r >= 0x80 {
					attrs += fmt.Sprintf(` textLength="%.2f" lengthAdjust="spacingAndGlyphs"`, cw)
				}
				fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" %s xml:space="preserve">%s</text>`, x, y+lh*0.74, attrs, esc(string(text)))
			}
			i = j
		}
		b.WriteString("\n")
	}
	for col, pb := range paths {
		fmt.Fprintf(&b, `<path d="%s" stroke="%s" stroke-width="1.3" fill="none"/>`+"\n", pb.String(), col)
	}
	b.WriteString("</svg>\n")
	os.Stdout.WriteString(b.String())
}
