package extract

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// minimalPDF builds a one-page PDF whose content stream shows text, with a
// correct cross-reference table.
func minimalPDF(lines ...string) []byte {
	var content strings.Builder
	content.WriteString("BT /F1 12 Tf 72 720 Td 14 TL\n")
	for _, l := range lines {
		fmt.Fprintf(&content, "(%s) Tj T*\n", l)
	}
	content.WriteString("ET")
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestText(t *testing.T) {
	docx := zipOf(t, map[string]string{"word/document.xml": `<w:document xmlns:w="w"><w:body>
<w:p><w:r><w:t>Crossbows are legal</w:t></w:r><w:r><w:t xml:space="preserve"> for archery.</w:t></w:r></w:p>
<w:p><w:r><w:t>Second</w:t><w:tab/><w:t>para</w:t></w:r></w:p></w:body></w:document>`})
	pptx := zipOf(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>Ten</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>Two</a:t></a:r></a:p></p:sld>`,
	})
	for _, c := range []struct {
		name string
		data []byte
		kind string
		want []string
		err  string
	}{
		{"notes.md", []byte("\xef\xbb\xbf# Title\r\n\r\n\r\nBody  \n"), KindText, []string{"# Title\n\nBody"}, ""},
		{"page.html", []byte("<html><title>T</title><body><h1>Head</h1><script>x()</script><p>Para</p></body></html>"), KindHTML, []string{"# Head", "Para"}, ""},
		{"r.docx", docx, KindDOCX, []string{"Crossbows are legal for archery.\nSecond\tpara"}, ""},
		{"deck.pptx", pptx, KindPPTX, []string{"## Slide 1\nTwo", "## Slide 2\nTen"}, ""},
		{"regs.pdf", minimalPDF("Turkey season opens Oct 1", "Limit one tom"), KindPDF, []string{"Turkey season opens Oct 1", "Limit one tom"}, ""},
		{"scan.pdf", minimalPDF(), KindPDF, nil, "no text layer"},
		{"photo.jpg", []byte{0xff, 0xd8, 0xff, 0x00, 0x10}, "", nil, "not a text"},
		{"old.doc", []byte("x"), "", nil, "save it as .docx"},
		{"sheet.xlsx", []byte("x"), "", nil, "CSV"},
		{"bad.docx", []byte("not a zip"), KindDOCX, nil, "not a valid Office file"},
	} {
		kind, text, err := Text(c.name, c.data)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || kind != c.kind {
			t.Errorf("%s: kind %q err %v", c.name, kind, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: text %q missing %q", c.name, text, w)
			}
		}
	}
}
