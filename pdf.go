package main

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
)

// Minimaler PDF-Erzeuger (A4, Helvetica, WinAnsi) – reicht für Berichte mit Text, Tabellen und Flächen.

type pdfDoc struct {
	pages []*bytes.Buffer
	cur   *bytes.Buffer
	W, H  float64
}

func newPDF() *pdfDoc { d := &pdfDoc{W: 595.28, H: 841.89}; d.AddPage(); return d }

func (d *pdfDoc) AddPage() {
	d.cur = &bytes.Buffer{}
	d.pages = append(d.pages, d.cur)
}

func (d *pdfDoc) PageNo() int { return len(d.pages) }

type rgb struct{ R, G, B float64 }

func hexColor(h string) rgb {
	var r, g, b int
	fmt.Sscanf(strings.TrimPrefix(h, "#"), "%02x%02x%02x", &r, &g, &b)
	return rgb{float64(r) / 255, float64(g) / 255, float64(b) / 255}
}

// Koordinaten: y von oben gemessen (wie am Bildschirm).
func (d *pdfDoc) Rect(x, y, w, h float64, fill rgb) {
	fmt.Fprintf(d.cur, "%.3f %.3f %.3f rg %.2f %.2f %.2f %.2f re f\n", fill.R, fill.G, fill.B, x, d.H-y-h, w, h)
}

func (d *pdfDoc) Line(x1, y1, x2, y2, width float64, c rgb) {
	fmt.Fprintf(d.cur, "%.3f %.3f %.3f RG %.2f w %.2f %.2f m %.2f %.2f l S\n", c.R, c.G, c.B, width, x1, d.H-y1, x2, d.H-y2)
}

// Text setzt die Grundlinie bei y. align: 'l', 'r', 'c'.
func (d *pdfDoc) Text(x, y float64, size float64, bold bool, c rgb, align byte, s string) {
	w := textWidth(s, size, bold)
	switch align {
	case 'r':
		x -= w
	case 'c':
		x -= w / 2
	}
	f := "F1"
	if bold {
		f = "F2"
	}
	fmt.Fprintf(d.cur, "BT %.3f %.3f %.3f rg /%s %.1f Tf %.2f %.2f Td (%s) Tj ET\n", c.R, c.G, c.B, f, size, x, d.H-y, pdfEscape(toWinAnsi(s)))
}

// Fit kürzt einen Text mit „…“, bis er in maxW passt.
func fitText(s string, size float64, bold bool, maxW float64) string {
	if textWidth(s, size, bold) <= maxW {
		return s
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		t := strings.TrimSpace(string(r)) + "…"
		if textWidth(t, size, bold) <= maxW {
			return t
		}
	}
	return "…"
}

// Helvetica-Breiten (1/1000 em) für ASCII 32..126.
var helvW = [95]int{278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
	1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
	333, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
	556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584}

// Helvetica-Bold-Breiten für ASCII 32..126.
var helvBW = [95]int{278, 333, 474, 556, 556, 889, 722, 238, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 333, 333, 584, 584, 584, 611,
	975, 722, 722, 722, 722, 667, 611, 778, 722, 278, 556, 722, 611, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 333, 278, 333, 584, 556,
	333, 556, 611, 556, 611, 556, 333, 611, 611, 278, 278, 556, 278, 889, 611, 611,
	611, 611, 389, 556, 333, 611, 556, 778, 556, 556, 500, 389, 280, 389, 584}

func textWidth(s string, size float64, bold bool) float64 {
	tbl := &helvW
	if bold {
		tbl = &helvBW
	}
	w := 0
	for _, r := range s {
		switch {
		case r >= 32 && r <= 126:
			w += tbl[r-32]
		case r == 'ä' || r == 'ö' || r == 'ü' || r == 'ß':
			w += 556
			if bold {
				w += 55
			}
		case r == 'Ä' || r == 'Ö' || r == 'Ü':
			w += 722
		case r == '–' || r == '€':
			w += 556
		case r == '…' || r == '—':
			w += 1000
		case r == '·' || r == '•':
			w += 350
		default:
			w += 556
		}
	}
	return float64(w) * size / 1000
}

var winAnsiExtra = map[rune]byte{'€': 0x80, '‚': 0x82, '„': 0x84, '…': 0x85, '‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94,
	'•': 0x95, '–': 0x96, '—': 0x97, '™': 0x99, '×': 0xD7, '✓': 'x', '→': '>', '∅': 'O'}

func toWinAnsi(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 128:
			out = append(out, byte(r))
		case r >= 0xA0 && r <= 0xFF:
			out = append(out, byte(r))
		default:
			if b, ok := winAnsiExtra[r]; ok {
				out = append(out, b)
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

func pdfEscape(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch c {
		case '(', ')', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func (d *pdfDoc) Bytes(title string) []byte {
	var b bytes.Buffer
	var offs []int
	obj := func(s string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), s)
	}
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	n := len(d.pages)
	// 1 Katalog, 2 Seiten, 3 F1, 4 F2, 5 Info, danach je Seite: Page + Inhalt
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, n)
	for i := range d.pages {
		kids[i] = fmt.Sprintf("%d 0 R", 6+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	obj(fmt.Sprintf("<< /Title (%s) /Producer (PingWatch) >>", pdfEscape(toWinAnsi(title))))
	for i, p := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>", d.W, d.H, 7+2*i))
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(p.Bytes())
		zw.Close()
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", len(offs), z.Len())
		b.Write(z.Bytes())
		b.WriteString("\nendstream\nendobj\n")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info 5 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.Bytes()
}
