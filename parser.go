package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// --- EPUB parsing ---

type opfPackage struct {
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Toc   string `xml:"toc,attr"` // EPUB2: manifest id of the .ncx file
		Items []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
	Metadata struct {
		Title string `xml:"title"`
	} `xml:"metadata"`
}

// TOCEntry is one entry from the epub's own table of contents (EPUB3
// nav.xhtml or EPUB2 toc.ncx) — the structure a real e-reader uses to build
// its chapter list and numbering, independent of whatever heading
// conventions the book's own body text happens to use.
type TOCEntry struct {
	Label      string `json:"label"`
	Href       string `json:"href"`
	SpineIndex int    `json:"spine_index"` // 1-based position in our parsed chapters array; 0 if unmatched
}

type ncxDoc struct {
	NavMap struct {
		NavPoints []ncxNavPoint `xml:"navPoint"`
	} `xml:"navMap"`
}

type ncxNavPoint struct {
	NavLabel struct {
		Text string `xml:"text"`
	} `xml:"navLabel"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	NavPoints []ncxNavPoint `xml:"navPoint"`
}

func (n ncxNavPoint) flatten(out *[]TOCEntry) {
	label := strings.TrimSpace(n.NavLabel.Text)
	if label != "" || n.Content.Src != "" {
		*out = append(*out, TOCEntry{Label: label, Href: n.Content.Src})
	}
	for _, child := range n.NavPoints {
		child.flatten(out)
	}
}

// extractTOC reads the epub's own declared table of contents (EPUB3 nav
// document or EPUB2 NCX) and maps each entry to the corresponding position
// in a chapters slice already parsed by parseEpub via spine order, matching
// on href (ignoring any #fragment). Returns an error if the epub has no
// navigable TOC at all.
func extractTOC(data []byte, chapters []Chapter) ([]TOCEntry, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a valid epub zip: %w", err)
	}

	var opfPath string
	for _, f := range r.File {
		if f.Name == "META-INF/container.xml" {
			rc, _ := f.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			re := regexp.MustCompile(`full-path="([^"]+)"`)
			if m := re.FindSubmatch(content); m != nil {
				opfPath = string(m[1])
			}
			break
		}
	}
	if opfPath == "" {
		return nil, fmt.Errorf("no OPF found in epub")
	}

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

	baseDir := ""
	if idx := strings.LastIndex(opfPath, "/"); idx >= 0 {
		baseDir = opfPath[:idx+1]
	}
	idToHref := make(map[string]string)
	idToProps := make(map[string]string)
	for _, item := range pkg.Manifest.Items {
		idToHref[item.ID] = baseDir + item.Href
		idToProps[item.ID] = item.Properties
	}

	fileMap := make(map[string]*zip.File)
	for _, f := range r.File {
		fileMap[f.Name] = f
	}

	readFile := func(path string) ([]byte, bool) {
		zf, ok := fileMap[path]
		if !ok {
			return nil, false
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		content, _ := io.ReadAll(rc)
		return content, true
	}

	var entries []TOCEntry
	var tocDir string // directory containing whichever nav/ncx file we used, for resolving its relative hrefs

	// EPUB3: manifest item with properties="nav"
	var navPath string
	for id, props := range idToProps {
		if strings.Contains(props, "nav") {
			navPath = idToHref[id]
			break
		}
	}
	if navPath != "" {
		if content, ok := readFile(navPath); ok {
			entries = parseNavXHTML(content)
			if idx := strings.LastIndex(navPath, "/"); idx >= 0 {
				tocDir = navPath[:idx+1]
			}
		}
	}

	// EPUB2 fallback: toc.ncx referenced by spine's toc attribute
	if len(entries) == 0 && pkg.Spine.Toc != "" {
		if ncxPath, ok := idToHref[pkg.Spine.Toc]; ok {
			if content, ok := readFile(ncxPath); ok {
				var doc ncxDoc
				if xml.Unmarshal(content, &doc) == nil {
					for _, np := range doc.NavMap.NavPoints {
						np.flatten(&entries)
					}
					if idx := strings.LastIndex(ncxPath, "/"); idx >= 0 {
						tocDir = ncxPath[:idx+1]
					}
				}
			}
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no nav.xhtml or toc.ncx found in epub")
	}

	// Match each TOC entry's href (resolved relative to the nav/ncx file's
	// own directory, ignoring any #fragment) to the already-parsed chapters
	// array. This must use chapters[i].Href (parseEpub's own record of which
	// file each surviving chapter came from) rather than recomputing spine
	// position from the raw OPF spine list — parseEpub silently drops some
	// spine items (covers, nav pages, anything under 150 chars), so raw
	// spine position and the filtered chapters array's Index can diverge.
	// Multiple TOC entries can legitimately point at the same spine file
	// (e.g. an anchor mid-chapter); each still resolves to that chapter's
	// position.
	hrefToSpineIndex := make(map[string]int)
	for _, ch := range chapters {
		if ch.Href != "" {
			hrefToSpineIndex[ch.Href] = ch.Index
		}
	}
	for i := range entries {
		file := entries[i].Href
		if idx := strings.IndexByte(file, '#'); idx >= 0 {
			file = file[:idx]
		}
		entries[i].SpineIndex = hrefToSpineIndex[tocDir+file]
	}

	return entries, nil
}

// parseNavXHTML extracts the <nav epub:type="toc"> list from an EPUB3
// navigation document as an ordered, flattened list of {label, href}.
func parseNavXHTML(content []byte) []TOCEntry {
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		return nil
	}
	var tocNav *html.Node
	var findNav func(*html.Node)
	findNav = func(n *html.Node) {
		if tocNav != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "nav" {
			for _, a := range n.Attr {
				if (a.Key == "type" || strings.HasSuffix(a.Key, ":type")) && a.Val == "toc" {
					tocNav = n
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findNav(c)
		}
	}
	findNav(doc)
	if tocNav == nil {
		return nil
	}
	var entries []TOCEntry
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, a := range n.Attr {
				if a.Key == "href" {
					href = a.Val
				}
			}
			if href != "" {
				entries = append(entries, TOCEntry{Label: strings.TrimSpace(nodeText(n)), Href: href})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(tocNav)
	return entries
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
	// Some books number their headings independently of the epub spine
	// (e.g. unnumbered front matter — title page, dedication, a table of
	// contents our own fallback mislabels "Chapter N" — pushes the real
	// "Chapter 1" several spine positions later), so a heading like
	// "10. Bob – August 10, 2133" doesn't match our own array position.
	// Capture that embedded number so it can be used as the book's real,
	// author-assigned chapter number instead of our spine index.
	leadingNumberRe := regexp.MustCompile(`^(\d+)\.\s*`)

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

		rawTitle, text := extractHTMLText(content)
		rawTitle = strings.TrimSpace(rawTitle)
		text = strings.TrimSpace(text)

		chapterNum := 0
		if m := leadingNumberRe.FindStringSubmatch(rawTitle); m != nil {
			chapterNum, _ = strconv.Atoi(m[1])
		}
		title := strings.TrimSpace(leadingNumberRe.ReplaceAllString(rawTitle, ""))
		// HTML text extraction doesn't distinguish the heading from the body,
		// so the heading (number included) is duplicated as the first line
		// of the extracted text (e.g. "10. Bob – August 10, 2133" followed by
		// the actual chapter). Strip it so that number doesn't also reach
		// Claude embedded in the content itself.
		if rawTitle != "" && strings.HasPrefix(text, rawTitle) {
			text = strings.TrimSpace(strings.TrimPrefix(text, rawTitle))
		}

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
			Index:  len(chapters) + 1,
			Number: chapterNum,
			Href:   href,
			Title:  title,
			Text:   text,
		})
	}

	// Re-number and clean titles
	for i := range chapters {
		if chapters[i].Title == "" {
			chapters[i].Title = fmt.Sprintf("Section %d", i+1)
		}
		chapters[i].Index = i + 1
	}

	// Prefer the book's own declared table of contents (nav.xhtml/toc.ncx)
	// for chapter numbering — it's the general, book-agnostic source every
	// e-reader uses, unlike the in-body heading heuristic above which only
	// works for books that happen to print their own chapter number in the
	// text. Falls back to that heuristic (already applied above) if the
	// epub has no usable TOC at all.
	if entries, err := extractTOC(data, chapters); err == nil && len(entries) > 0 {
		applyTOCNumbering(chapters, entries)
	}

	return chapters, nil
}

// chapterNumberFromLabel extracts a chapter number from a TOC label, but
// only for patterns unambiguous enough to trust: the whole label is just a
// number ("10", "10."), an explicit "Chapter" prefix ("Chapter 10: ..."), or
// a leading "N. " convention ("10. Title"). This deliberately does not match
// an arbitrary digit anywhere in the label — e.g. a story-internal codename
// like "SCP-055" or a date embedded in the title — to avoid mistaking an
// unrelated number for the chapter number.
var (
	bareNumberLabelRe = regexp.MustCompile(`^(\d+)\.?\s*$`)
	chapterPrefixRe   = regexp.MustCompile(`(?i)^chapter\s+(\d+)\b`)
	numberedTitleRe   = regexp.MustCompile(`^(\d+)\.\s+\S`)

	chapterPrefixStripRe = regexp.MustCompile(`(?i)^chapter\s+\d+\s*[:\-–—]?\s*`)
	numberedTitleStripRe = regexp.MustCompile(`^\d+\.\s+`)
	autoTitleRe          = regexp.MustCompile(`^(Chapter|Section) \d+$`)
)

func chapterNumberFromLabel(label string) (int, bool) {
	label = strings.TrimSpace(label)
	for _, re := range []*regexp.Regexp{bareNumberLabelRe, chapterPrefixRe, numberedTitleRe} {
		if m := re.FindStringSubmatch(label); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// cleanTOCLabelTitle strips a recognized chapter-number prefix from a TOC
// label so it can safely replace an auto-generated placeholder title (our
// own "Chapter N"/"Section N" fallback, which reflects spine position rather
// than the book's real chapter number) without reintroducing a conflicting
// number into the title text.
func cleanTOCLabelTitle(label string, num int) string {
	label = strings.TrimSpace(label)
	if bareNumberLabelRe.MatchString(label) {
		return fmt.Sprintf("Chapter %d", num)
	}
	for _, re := range []*regexp.Regexp{chapterPrefixStripRe, numberedTitleStripRe} {
		if m := re.FindString(label); m != "" {
			cleaned := strings.TrimSpace(strings.TrimPrefix(label, m))
			if cleaned == "" {
				return fmt.Sprintf("Chapter %d", num)
			}
			return cleaned
		}
	}
	return label
}

// applyTOCNumbering assigns each chapter its real, reader-facing chapter
// number from the book's own table of contents. It prefers an explicit
// number found in a TOC label (the safest, most literal signal); entries
// without one are left unnumbered — e.g. an "Introduction" that has its own
// TOC entry but isn't part of the book's numbered chapter sequence simply
// falls back to raw spine position (chapterDisplayNumber's default), which
// is correct. Only when the book has no explicit numbering anywhere in its
// TOC does this fall back to numbering every matched entry sequentially, so
// unnumbered books (e.g. thematically-titled chapters) still get a chapter
// count that excludes front matter, rather than no signal at all.
func applyTOCNumbering(chapters []Chapter, entries []TOCEntry) {
	assigned := make(map[int]bool)
	anyLabelNumbered := false
	for _, e := range entries {
		if e.SpineIndex < 1 || e.SpineIndex > len(chapters) || assigned[e.SpineIndex] {
			continue
		}
		if n, ok := chapterNumberFromLabel(e.Label); ok {
			ch := &chapters[e.SpineIndex-1]
			ch.Number = n
			if autoTitleRe.MatchString(ch.Title) {
				ch.Title = cleanTOCLabelTitle(e.Label, n)
			}
			assigned[e.SpineIndex] = true
			anyLabelNumbered = true
		}
	}
	if anyLabelNumbered {
		return
	}
	num := 0
	for _, e := range entries {
		if e.SpineIndex < 1 || e.SpineIndex > len(chapters) || assigned[e.SpineIndex] {
			continue
		}
		num++
		chapters[e.SpineIndex-1].Number = num
		assigned[e.SpineIndex] = true
	}
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

// findChapterPosition searches cached chapters for the given text snippet and
// returns the 1-based chapter index plus the character offset within that
// chapter's text where the snippet ends — i.e. how far into the chapter the
// reader has actually gotten. Offset is -1 when only the chapter (not the
// exact position) could be determined, signaling the caller to treat the
// whole chapter as read rather than guess a cutoff.
func findChapterPosition(chapters []Chapter, snippet string) (int, int) {
	if len(snippet) < 10 {
		return 1, -1
	}
	snippet = strings.ToLower(strings.TrimSpace(snippet))
	// Try exact substring first — gives us a precise in-chapter offset.
	for _, ch := range chapters {
		lower := strings.ToLower(ch.Text)
		if idx := strings.Index(lower, snippet); idx >= 0 {
			return ch.Index, idx + len(snippet)
		}
	}
	// Fuzzy fallback: score by shared 6-grams. No reliable offset here.
	best, bestScore := 1, 0
	ngrams := makeNgrams(snippet, 6)
	for _, ch := range chapters {
		score := countSharedNgrams(strings.ToLower(ch.Text), ngrams)
		if score > bestScore {
			bestScore = score
			best = ch.Index
		}
	}
	return best, -1
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
