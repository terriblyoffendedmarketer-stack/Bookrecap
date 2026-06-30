package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// --- EPUB parsing ---

type opfPackage struct {
	Manifest struct {
		Items []struct {
			ID        string `xml:"id,attr"`
			Href      string `xml:"href,attr"`
			MediaType string `xml:"media-type,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Items []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
	Metadata struct {
		Title string `xml:"title"`
	} `xml:"metadata"`
}

func parseEpub(data []byte) ([]Chapter, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a valid epub zip: %w", err)
	}

	// Find container.xml to locate the OPF file
	var opfPath string
	for _, f := range r.File {
		if f.Name == "META-INF/container.xml" {
			rc, _ := f.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			// Extract rootfile path
			re := regexp.MustCompile(`full-path="([^"]+)"`)
			m := re.FindSubmatch(content)
			if m != nil {
				opfPath = string(m[1])
			}
			break
		}
	}
	if opfPath == "" {
		return nil, fmt.Errorf("no OPF found in epub")
	}

	// Parse OPF
	var pkg opfPackage
	for _, f := range r.File {
		if f.Name == opfPath {
			rc, _ := f.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			xml.Unmarshal(content, &pkg)
			break
		}
	}

	// Build id→href map
	idToHref := make(map[string]string)
	baseDir := ""
	if idx := strings.LastIndex(opfPath, "/"); idx >= 0 {
		baseDir = opfPath[:idx+1]
	}
	for _, item := range pkg.Manifest.Items {
		idToHref[item.ID] = baseDir + item.Href
	}

	// Build file lookup
	fileMap := make(map[string]*zip.File)
	for _, f := range r.File {
		fileMap[f.Name] = f
	}

	// Extract chapters in spine order
	var chapters []Chapter
	chapterRe := regexp.MustCompile(`(?i)chapter|part|section|prologue|epilogue|introduction|preface`)

	for _, spineItem := range pkg.Spine.Items {
		href, ok := idToHref[spineItem.IDRef]
		if !ok {
			continue
		}
		zf, ok := fileMap[href]
		if !ok {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			continue
		}
		content, _ := io.ReadAll(rc)
		rc.Close()

		title, text := extractHTMLText(content)
		text = strings.TrimSpace(text)
		if len(text) < 150 {
			continue // skip nav pages, covers, etc.
		}
		// Use chapter-like title if found, else skip non-chapter items
		if title == "" || !chapterRe.MatchString(title+" "+text[:min(200, len(text))]) {
			if len(chapters) > 0 || len(text) > 500 {
				if title == "" {
					title = fmt.Sprintf("Chapter %d", len(chapters)+1)
				}
			}
		}
		chapters = append(chapters, Chapter{
			Index: len(chapters) + 1,
			Title: title,
			Text:  text,
		})
	}

	// Re-number and clean titles
	for i := range chapters {
		if chapters[i].Title == "" {
			chapters[i].Title = fmt.Sprintf("Section %d", i+1)
		}
		chapters[i].Index = i + 1
	}

	return chapters, nil
}

func extractHTMLText(content []byte) (title, text string) {
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		return "", string(content)
	}
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			tag := n.Data
			// Capture first heading as title
			if title == "" && (tag == "h1" || tag == "h2" || tag == "h3") {
				title = strings.TrimSpace(nodeText(n))
			}
			// Skip script/style
			if tag == "script" || tag == "style" {
				return
			}
			// Block elements get a newline
			if isBlock(tag) && sb.Len() > 0 {
				sb.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, sb.String()
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "br", "tr":
		return true
	}
	return false
}

// --- PDF parsing (best-effort via pdftotext subprocess) ---

func parsePDF(data []byte) ([]Chapter, error) {
	text, err := pdfToText(data)
	if err != nil || strings.TrimSpace(text) == "" {
		return []Chapter{{Index: 1, Title: "Full Text", Text: "Could not extract text from this PDF. Try converting it to epub format for best results."}}, nil
	}
	return splitIntoChapters(text), nil
}

var chapterHeadingRe = regexp.MustCompile(`(?mi)^((?:Chapter|CHAPTER|Part|PART)\s+(?:[IVXLCDMivxlcdm]+|\d+)[^\n]{0,60})$`)

func splitIntoChapters(text string) []Chapter {
	locs := chapterHeadingRe.FindAllStringIndex(text, -1)
	if len(locs) < 2 {
		// No clear chapter structure — split into ~3000-word chunks
		words := strings.Fields(text)
		const chunkWords = 3000
		var chapters []Chapter
		for i := 0; i < len(words); i += chunkWords {
			end := i + chunkWords
			if end > len(words) {
				end = len(words)
			}
			chapters = append(chapters, Chapter{
				Index: len(chapters) + 1,
				Title: fmt.Sprintf("Section %d", len(chapters)+1),
				Text:  strings.Join(words[i:end], " "),
			})
		}
		return chapters
	}

	var chapters []Chapter
	for i, loc := range locs {
		title := strings.TrimSpace(text[loc[0]:loc[1]])
		var body string
		if i+1 < len(locs) {
			body = strings.TrimSpace(text[loc[1]:locs[i+1][0]])
		} else {
			body = strings.TrimSpace(text[loc[1]:])
		}
		chapters = append(chapters, Chapter{
			Index: len(chapters) + 1,
			Title: title,
			Text:  body,
		})
	}
	return chapters
}

// findChapterForText searches cached chapters for the given text snippet
// and returns the 1-based chapter index (or 1 if not found).
func findChapterForText(chapters []Chapter, snippet string) int {
	if len(snippet) < 10 {
		return 1
	}
	snippet = strings.ToLower(strings.TrimSpace(snippet))
	// Try exact substring first
	for _, ch := range chapters {
		if strings.Contains(strings.ToLower(ch.Text), snippet) {
			return ch.Index
		}
	}
	// Fuzzy: score by shared 6-grams
	best, bestScore := 1, 0
	ngrams := makeNgrams(snippet, 6)
	for _, ch := range chapters {
		score := countSharedNgrams(strings.ToLower(ch.Text), ngrams)
		if score > bestScore {
			bestScore = score
			best = ch.Index
		}
	}
	return best
}

func makeNgrams(s string, n int) map[string]bool {
	m := make(map[string]bool)
	runes := []rune(s)
	for i := 0; i <= len(runes)-n; i++ {
		m[string(runes[i:i+n])] = true
	}
	return m
}

func countSharedNgrams(text string, ngrams map[string]bool) int {
	runes := []rune(text)
	n := 6
	count := 0
	for i := 0; i <= len(runes)-n; i++ {
		if ngrams[string(runes[i:i+n])] {
			count++
		}
	}
	return count
}

func parseBook(data []byte, mimeType string) ([]Chapter, error) {
	if strings.Contains(mimeType, "epub") || (len(data) > 4 && string(data[:4]) == "PK\x03\x04") {
		return parseEpub(data)
	}
	return parsePDF(data)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
