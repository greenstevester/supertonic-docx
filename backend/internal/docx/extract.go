// Package docx extracts paragraph-level plain text from .docx files.
//
// A .docx is a ZIP containing word/document.xml. We parse that XML directly
// with encoding/xml — no CGO, no pandoc shell-out — and emit one string per
// <w:p> element. Empty and whitespace-only paragraphs are dropped.
//
// This is intentionally minimal: we don't preserve formatting, tables, or
// images. TTS only needs the linear text. If the upstream document has
// structure the user cares about (headings as section breaks, say), that
// can be layered on by extending paragraphKind below.
package docx

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Paragraph is one chunk of text destined for TTS.
type Paragraph struct {
	Index int    // 1-based, in document order, after empty-paragraph filtering
	Text  string // plain text, runs concatenated, leading/trailing whitespace stripped
}

// Extract returns the non-empty paragraphs from a .docx file at path.
func Extract(path string) ([]Paragraph, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open docx: %w", err)
	}
	defer zr.Close()

	var docXML io.ReadCloser
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML, err = f.Open()
			if err != nil {
				return nil, fmt.Errorf("open document.xml: %w", err)
			}
			break
		}
	}
	if docXML == nil {
		return nil, fmt.Errorf("not a valid docx: missing word/document.xml")
	}
	defer docXML.Close()

	return parseParagraphs(docXML)
}

// parseParagraphs streams document.xml and emits paragraphs.
//
// We walk the token stream rather than unmarshalling into a struct: docx
// XML is deeply nested and we only care about <w:p> and <w:t> elements.
// Streaming also keeps memory usage flat for large documents.
func parseParagraphs(r io.Reader) ([]Paragraph, error) {
	dec := xml.NewDecoder(r)

	var (
		out     []Paragraph
		inPara  bool
		inText  bool
		curPara strings.Builder
		idx     int
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				inPara = true
				curPara.Reset()
			case "t":
				if inPara {
					inText = true
				}
			case "tab":
				if inPara {
					curPara.WriteByte('\t')
				}
			case "br":
				if inPara {
					curPara.WriteByte(' ')
				}
			}

		case xml.CharData:
			if inText {
				curPara.Write(t)
			}

		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				inPara = false
				text := normalizeSpace(curPara.String())
				if text != "" {
					idx++
					out = append(out, Paragraph{Index: idx, Text: text})
				}
			}
		}
	}

	return out, nil
}

// normalizeSpace collapses runs of whitespace and trims the edges.
// docx often produces text like "  Hello\t\tworld  " from formatted runs.
func normalizeSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // treat leading whitespace as already-consumed
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\u00a0' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	out := b.String()
	return strings.TrimRight(out, " ")
}
