// Package extract turns uploaded files into plain text for the agents:
// text and code files as they are, HTML through the fetcher's readable-text
// extraction, Word (.docx) and PowerPoint (.pptx) from their XML, and PDF
// through its text layer. Anything else is refused with a reason the user
// can act on.
package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
)

// Kinds of extracted documents.
const (
	KindText = "text"
	KindHTML = "html"
	KindPDF  = "pdf"
	KindDOCX = "docx"
	KindPPTX = "pptx"
)

// MaxUnzipped bounds what is read out of a .docx or .pptx (zip bombs).
const MaxUnzipped = 64 << 20

// Text extracts a file's text. name is only used for its extension.
func Text(name string, data []byte) (kind, text string, err error) {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case ext == ".pdf" || bytes.HasPrefix(data, []byte("%PDF-")):
		kind = KindPDF
		text, err = pdfText(data)
	case ext == ".docx":
		kind = KindDOCX
		text, err = officeText(data, func(n string) bool { return n == "word/document.xml" }, "p", "t")
	case ext == ".pptx":
		kind = KindPPTX
		text, err = officeText(data, isSlide, "p", "t")
	case ext == ".doc" || ext == ".ppt":
		return "", "", fmt.Errorf("old Office format (%s): save it as .docx or .pptx and upload that", ext)
	case ext == ".xls" || ext == ".xlsx" || ext == ".ods":
		return "", "", errors.New("spreadsheets are not supported yet: export the sheet as CSV and upload that")
	case ext == ".html" || ext == ".htm" || looksHTML(data):
		kind = KindHTML
		_, text = fetch.HTMLText(string(data))
	case isText(data):
		kind = KindText
		text = strings.TrimPrefix(string(data), "\uFEFF")
	default:
		return "", "", errors.New("not a text, PDF, Word or PowerPoint file (images and other binary files are not supported)")
	}
	if err != nil {
		return kind, "", err
	}
	text = tidy(text)
	if text == "" {
		if kind == KindPDF {
			return kind, "", errors.New("the PDF has no text layer (it may be scanned images); run it through OCR first")
		}
		return kind, "", errors.New("no readable text in the file")
	}
	return kind, text, nil
}

// isText accepts valid UTF-8 without NUL bytes (code, Markdown, CSV, JSON,
// logs...).
func isText(b []byte) bool {
	return utf8.Valid(b) && !bytes.Contains(b, []byte{0})
}

func looksHTML(b []byte) bool {
	s := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(s, "<html") || strings.Contains(s, "<!doctype html")
}

var slideRE = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

func isSlide(n string) bool { return slideRE.MatchString(n) }

// officeText reads the matching XML parts of an Office zip, in order
// (slides by number), keeping paragraphs (para elements) on their own
// lines and the text of run elements (textEl).
func officeText(data []byte, want func(string) bool, para, textEl string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("not a valid Office file: %w", err)
	}
	var files []*zip.File
	for _, f := range zr.File {
		if want(f.Name) {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return slideNumber(files[i].Name) < slideNumber(files[j].Name) })
	var b strings.Builder
	budget := int64(MaxUnzipped)
	for i, f := range files {
		if isSlide(f.Name) {
			fmt.Fprintf(&b, "\n## Slide %d\n", i+1)
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		lr := &io.LimitedReader{R: rc, N: budget}
		err = xmlText(lr, para, textEl, &b)
		budget = lr.N
		rc.Close()
		if err != nil {
			return "", err
		}
		if budget <= 0 {
			return "", errors.New("the document is too large once unpacked")
		}
	}
	return b.String(), nil
}

func slideNumber(n string) int {
	if m := slideRE.FindStringSubmatch(n); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 0
}

// xmlText copies the character data of textEl elements, ending a line at
// the end of each para element; tabs and breaks keep their meaning.
func xmlText(r io.Reader, para, textEl string, b *strings.Builder) error {
	d := xml.NewDecoder(r)
	inText := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("could not read the document: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case textEl:
				inText = true
			case "tab":
				b.WriteString("\t")
			case "br", "cr":
				b.WriteString("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case textEl:
				inText = false
			case para:
				b.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
}

// pdfText reads a PDF's text layer. The parser can panic on malformed
// files, so a panic becomes an error.
func pdfText(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("could not read the PDF (%v)", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("could not read the PDF: %w", err)
	}
	var b strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		rows, err := p.GetTextByRow()
		if err != nil {
			continue // a broken page should not lose the rest
		}
		fmt.Fprintf(&b, "\n## Page %d\n", i)
		for _, row := range rows {
			line := make([]string, 0, len(row.Content))
			for _, t := range row.Content {
				line = append(line, t.S)
			}
			b.WriteString(strings.Join(line, ""))
			b.WriteString("\n")
		}
		if b.Len() > MaxUnzipped {
			return "", errors.New("the PDF is too large")
		}
	}
	// A PDF whose pages carry no text at all has only page headings.
	if strings.TrimSpace(pageHeadingRE.ReplaceAllString(b.String(), "")) == "" {
		return "", nil
	}
	return b.String(), nil
}

var pageHeadingRE = regexp.MustCompile(`(?m)^## (Page|Slide) \d+$`)

// tidy trims lines and collapses runs of blank lines.
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	blank := false
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
