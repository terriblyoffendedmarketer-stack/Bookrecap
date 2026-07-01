package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/securecookie"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
)

//go:embed frontend
var frontendFS embed.FS

var sc *securecookie.SecureCookie

func initSC() {
	cfg := loadConfig()
	sc = securecookie.New(
		[]byte(padKey(cfg.SecretKey, 32)),
		[]byte(padKey(cfg.SecretBlockKey, 32)),
	)
}

func padKey(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat("0", n-len(s))
}

// --- Session ---

type Session struct {
	Token *oauth2.Token `json:"token"`
	Email string        `json:"email"`
}

func getSession(r *http.Request) *Session {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	var sess Session
	if err := sc.Decode("session", cookie.Value, &sess); err != nil {
		return nil
	}
	return &sess
}

func setSession(w http.ResponseWriter, sess *Session) {
	encoded, err := sc.Encode("session", sess)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 30,
	})
}

func requireSession(w http.ResponseWriter, r *http.Request) *Session {
	sess := getSession(r)
	if sess == nil || sess.Token == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return nil
	}
	return sess
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
}

func flusher(w http.ResponseWriter) func() {
	if f, ok := w.(http.Flusher); ok {
		return f.Flush
	}
	return func() {}
}

// --- Auth routes ---

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	state := fmt.Sprintf("%d", time.Now().UnixNano())
	http.Redirect(w, r, getAuthURL(state), http.StatusFound)
}

func handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	token, err := exchangeCode(code)
	if err != nil {
		http.Error(w, "oauth exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	email := getUserEmail(token)
	setSession(w, &Session{Token: token, Email: email})
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	if sess == nil || sess.Token == nil {
		writeJSON(w, map[string]interface{}{"authed": false})
		return
	}
	writeJSON(w, map[string]interface{}{"authed": true, "email": sess.Email})
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "session", MaxAge: -1, Path: "/"})
	http.Redirect(w, r, "/", http.StatusFound)
}

// --- Book API ---

func handleSearch(w http.ResponseWriter, r *http.Request) {
	sess := requireSession(w, r)
	if sess == nil {
		return
	}
	q := r.URL.Query().Get("q")
	files, err := searchBooks(sess.Token, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}

func handleContext(w http.ResponseWriter, r *http.Request) {
	sess := requireSession(w, r)
	if sess == nil {
		return
	}
	var body struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MimeType string `json:"mime_type"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	chapters, ok := getChapters(body.FileID)
	if !ok {
		data, mimeType, err := downloadFile(sess.Token, body.FileID)
		if err != nil {
			http.Error(w, "download failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if mimeType == "" {
			mimeType = body.MimeType
		}
		chapters, err = parseBook(data, mimeType)
		if err != nil {
			http.Error(w, "parse failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		setChapters(body.FileID, body.FileName, chapters)
		if strings.Contains(mimeType, "epub") {
			setRawEpub(body.FileID, data)
		}

		// Generate per-chapter summaries so subsequent requests can use smart context.
		log.Printf("generating summaries for %d chapters of %q", len(chapters), body.FileName)
		summaries := SummarizeChapters(chapters)
		setSummaries(body.FileID, summaries)
		log.Printf("summaries ready for %q", body.FileName)
	} else if _, hasSummaries := getSummaries(body.FileID); !hasSummaries {
		// Cached book missing summaries (e.g. from before this feature) — generate now.
		log.Printf("backfilling summaries for cached book %q", body.FileName)
		summaries := SummarizeChapters(chapters)
		setSummaries(body.FileID, summaries)
	}

	titles := make([]string, len(chapters))
	for i, ch := range chapters {
		titles[i] = ch.Title
	}
	// Prefer the book's own printed chapter count (excludes front matter like
	// title page, dedication, or a table of contents) so the reader's chapter
	// picker matches what they see in the book itself, not our internal spine
	// array length.
	chapterCount := realChapterCount(chapters)
	if chapterCount == 0 {
		chapterCount = len(chapters)
	}
	writeJSON(w, map[string]interface{}{
		"chapter_count": chapterCount,
		"chapters":      titles,
	})
}

func handleRecap(w http.ResponseWriter, r *http.Request) {
	sess := requireSession(w, r)
	if sess == nil {
		return
	}
	var body struct {
		FileID       string `json:"file_id"`
		Title        string `json:"title"`
		ChapterCount int    `json:"chapter_count"`
		FromChapter  int    `json:"from_chapter"` // 0/1 = full history; >1 = windowed recap
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	chapters, ok := getChapters(body.FileID)
	if !ok {
		http.Error(w, "book not loaded — call /api/context first", http.StatusBadRequest)
		return
	}
	summaries, _ := getSummaries(body.FileID)

	// body.ChapterCount/FromChapter are the book's own printed chapter
	// numbers (what the reader actually sees), which can diverge from our
	// internal spine array position when front matter shifts them — resolve
	// to the correct spine boundary before slicing. FromChapter <= 1 means
	// "full history" and is passed through as-is.
	spineUpTo := resolveSpineUpTo(chapters, body.ChapterCount)
	spineFrom := body.FromChapter
	if spineFrom > 1 {
		spineFrom = resolveSpineUpTo(chapters, spineFrom)
	}
	log.Printf("recap: fileID=%s realUpTo=%d spineUpTo=%d realFrom=%d spineFrom=%d chapters=%d summaries=%d", body.FileID, body.ChapterCount, spineUpTo, body.FromChapter, spineFrom, len(chapters), len(summaries))
	sseHeaders(w)
	flush := flusher(w)
	if err := StreamRecap(w, flush, body.Title, chapters, summaries, spineUpTo, spineFrom); err != nil {
		log.Printf("recap stream error: %v", err)
	}
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	sess := requireSession(w, r)
	if sess == nil {
		return
	}
	var body struct {
		FileID       string          `json:"file_id"`
		Title        string          `json:"title"`
		ChapterCount int             `json:"chapter_count"`
		Messages     []claudeMessage `json:"messages"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	chapters, ok := getChapters(body.FileID)
	if !ok {
		http.Error(w, "book not loaded — call /api/context first", http.StatusBadRequest)
		return
	}
	summaries, _ := getSummaries(body.FileID)
	spineUpTo := resolveSpineUpTo(chapters, body.ChapterCount)
	log.Printf("chat: fileID=%s realUpTo=%d spineUpTo=%d chapters=%d summaries=%d", body.FileID, body.ChapterCount, spineUpTo, len(chapters), len(summaries))
	sseHeaders(w)
	flush := flusher(w)
	if err := StreamChat(w, flush, body.Title, chapters, summaries, spineUpTo, body.Messages); err != nil {
		log.Printf("chat stream error: %v", err)
	}
}

// handleDebug reports what buildContextSmart/buildContext actually produce
// for a given file_id and chapter, so we can tell whether the chapter cap
// bug is caused by missing summaries (fallback to the truncating buildContext)
// or something else. No auth — temporary diagnostic endpoint.
func handleDebug(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")
	chapter, _ := strconv.Atoi(r.URL.Query().Get("chapter"))

	var bookTitle string
	usedLatest := false
	if fileID == "" {
		if fid, title, ok := latestBook(); ok {
			fileID, bookTitle, usedLatest = fid, title, true
		}
	}

	chapters, chaptersCached := getChapters(fileID)
	summaries, summariesCached := getSummaries(fileID)

	resp := map[string]interface{}{
		"used_latest_book": usedLatest,
		"book_title":       bookTitle,
		"chapters_cached":  chaptersCached,
		"summaries_cached": summariesCached,
		"summary_count":    len(summaries),
		"chapter_count":    len(chapters),
	}

	if chaptersCached {
		titles := make([]string, len(chapters))
		for i, ch := range chapters {
			titles[i] = fmt.Sprintf("spine %d / chapter %d: %s", ch.Index, chapterDisplayNumber(ch), ch.Title)
		}
		resp["all_chapter_titles"] = titles
		resp["real_chapter_count"] = realChapterCount(chapters)
	}

	if chaptersCached {
		// `chapter` is interpreted the same way the real app does: the
		// book's own printed chapter number, resolved to the correct spine
		// boundary (see resolveSpineUpTo) rather than treated as a raw spine
		// array index.
		upTo := len(chapters)
		if chapter >= 1 {
			upTo = resolveSpineUpTo(chapters, chapter)
		}
		resp["requested_real_chapter"] = chapter
		safe := extractChapters(chapters, upTo)
		safeSummaries := summaries
		if len(safeSummaries) > upTo {
			safeSummaries = safeSummaries[:upTo]
		}
		var ctx string
		if len(safeSummaries) > 0 {
			ctx = buildContextSmart(safeSummaries, safe, min(10, len(safe)), 8_000)
		} else {
			ctx = buildContext(safe, 100_000, 8_000)
		}
		preview := ctx
		if len(preview) > 500 {
			preview = preview[:500]
		}
		resp["resolved_upTo"] = upTo
		resp["chapter_label"] = chapterLabel(chapters, upTo)
		resp["context_length"] = len(ctx)
		resp["context_preview"] = preview
	}

	writeJSON(w, resp)
}

// handleDebugChapter returns the full raw title+text of a single cached
// chapter by its spine index, so the actual book content can be inspected
// directly over HTTP without needing separate Google Drive access. No auth —
// temporary diagnostic endpoint.
func handleDebugChapter(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")
	index, _ := strconv.Atoi(r.URL.Query().Get("index"))

	if fileID == "" {
		if fid, _, ok := latestBook(); ok {
			fileID = fid
		}
	}

	chapters, ok := getChapters(fileID)
	if !ok {
		writeJSON(w, map[string]interface{}{"error": "no chapters cached"})
		return
	}
	if index < 1 || index > len(chapters) {
		writeJSON(w, map[string]interface{}{"error": fmt.Sprintf("index out of range (1-%d)", len(chapters))})
		return
	}
	ch := chapters[index-1]
	writeJSON(w, map[string]interface{}{
		"index":         ch.Index,
		"number":        ch.Number,
		"title":         ch.Title,
		"text_length":   len(ch.Text),
		"text":          ch.Text,
		"chapter_count": len(chapters),
	})
}

// handleDebugTOC parses the epub's own declared table of contents (EPUB3
// nav.xhtml or EPUB2 toc.ncx) and reports how each entry maps to our parsed
// spine-ordered chapters array. This is the general, book-agnostic source of
// chapter structure a real e-reader uses — validating it here before relying
// on it, rather than the heading-text heuristics that only work for books
// which happen to print their own chapter number in the text. No auth —
// temporary diagnostic endpoint.
func handleDebugTOC(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")
	if fileID == "" {
		if fid, _, ok := latestBook(); ok {
			fileID = fid
		}
	}

	raw, ok := getRawEpub(fileID)
	if !ok {
		writeJSON(w, map[string]interface{}{"error": "no raw epub cached for this book — reopen it in the app first"})
		return
	}
	chapters, _ := getChapters(fileID)

	entries, err := extractTOC(raw, chapters)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}

	rows := make([]map[string]interface{}, len(entries))
	for i, e := range entries {
		row := map[string]interface{}{
			"toc_position": i + 1,
			"label":        e.Label,
			"href":         e.Href,
			"spine_index":  e.SpineIndex,
		}
		if e.SpineIndex >= 1 && e.SpineIndex <= len(chapters) {
			row["matched_chapter_title"] = chapters[e.SpineIndex-1].Title
		}
		rows[i] = row
	}
	writeJSON(w, map[string]interface{}{
		"toc_entry_count": len(entries),
		"entries":         rows,
	})
}

// detectChapter resolves which chapter the photo corresponds to, plus how far
// into that chapter's text the reader has gotten (a character offset, or -1
// if the whole chapter should be treated as read). spineHint, if >= 1, is a
// spine array position already resolved from the reader's manually-entered
// chapter count (see resolveSpineUpTo) and is taken as-is — the whole chapter
// is fair game. Otherwise the image is sent to Claude for OCR and the
// extracted text is matched against cached chapter content to find the exact
// in-chapter position, preventing spoilers from later in the same chapter.
func detectChapter(chapters []Chapter, imageB64, mediaType string, spineHint int) (int, int) {
	if spineHint >= 1 {
		return spineHint, -1
	}
	upTo, offset := len(chapters), -1
	if snippet, err := IdentifyChapterFromImage(imageB64, mediaType); err == nil && snippet != "" {
		if n, off := findChapterPosition(chapters, snippet); n > 0 {
			upTo, offset = n, off
		}
	}
	return upTo, offset
}

func handlePhoto(w http.ResponseWriter, r *http.Request) {
	sess := requireSession(w, r)
	if sess == nil {
		return
	}
	var body struct {
		FileID       string `json:"file_id"`
		Title        string `json:"title"`
		ChapterCount int    `json:"chapter_count"` // -1 means "auto-detect from photo"
		ImageB64     string `json:"image_b64"`
		MediaType    string `json:"media_type"`
		Question     string `json:"question"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.MediaType == "" {
		body.MediaType = "image/jpeg"
	}
	if body.Question == "" {
		body.Question = "What's happening in this passage? Explain it in plain English."
	}

	chapters, ok := getChapters(body.FileID)
	if !ok {
		http.Error(w, "book not loaded — call /api/context first", http.StatusBadRequest)
		return
	}
	summaries, _ := getSummaries(body.FileID)

	spineHint := -1
	if body.ChapterCount >= 1 {
		spineHint = resolveSpineUpTo(chapters, body.ChapterCount)
	}
	upTo, offset := detectChapter(chapters, body.ImageB64, body.MediaType, spineHint)
	safeChapters := extractChaptersPartial(chapters, upTo, offset)

	sseHeaders(w)
	fmt.Fprintf(w, "data: {\"chapter_detected\":%d}\n\n", chapterDisplayNumber(chapters[upTo-1]))
	flusher(w)()

	flush := flusher(w)
	if err := StreamPhoto(w, flush, body.Title, safeChapters, summaries, len(safeChapters), body.ImageB64, body.MediaType, body.Question); err != nil {
		log.Printf("photo stream error: %v", err)
	}
}

// handlePhotoRecap does a full recap based on chapter auto-detected from photo.
func handlePhotoRecap(w http.ResponseWriter, r *http.Request) {
	sess := requireSession(w, r)
	if sess == nil {
		return
	}
	var body struct {
		FileID    string `json:"file_id"`
		Title     string `json:"title"`
		ImageB64  string `json:"image_b64"`
		MediaType string `json:"media_type"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.MediaType == "" {
		body.MediaType = "image/jpeg"
	}

	chapters, ok := getChapters(body.FileID)
	if !ok {
		http.Error(w, "book not loaded — call /api/context first", http.StatusBadRequest)
		return
	}
	summaries, _ := getSummaries(body.FileID)

	upTo, offset := detectChapter(chapters, body.ImageB64, body.MediaType, -1)
	safeChapters := extractChaptersPartial(chapters, upTo, offset)

	sseHeaders(w)
	fmt.Fprintf(w, "data: {\"chapter_detected\":%d}\n\n", chapterDisplayNumber(chapters[upTo-1]))
	flusher(w)()

	flush := flusher(w)
	if err := StreamRecap(w, flush, body.Title, safeChapters, summaries, len(safeChapters), 0); err != nil {
		log.Printf("photo recap stream error: %v", err)
	}
}

// --- Config API ---

func handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := loadConfig()
	// Never send secret keys; mask the client secret and API key after the first 4 chars
	writeJSON(w, map[string]interface{}{
		"google_client_id":     cfg.GoogleClientID,
		"google_client_secret": mask(cfg.GoogleClientSecret),
		"google_redirect_uri":  cfg.GoogleRedirectURI,
		"anthropic_api_key":    mask(cfg.AnthropicAPIKey),
		"drive_folder_id":      cfg.DriveFolderID,
		"ready":                configReady(),
	})
}

func handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GoogleClientID     string `json:"google_client_id"`
		GoogleClientSecret string `json:"google_client_secret"`
		GoogleRedirectURI  string `json:"google_redirect_uri"`
		AnthropicAPIKey    string `json:"anthropic_api_key"`
		DriveFolderID      string `json:"drive_folder_id"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.GoogleClientID != "" {
		setConfigVal("google_client_id", body.GoogleClientID)
	}
	// Only overwrite secret/key if user sent a real value (not a masked placeholder)
	if body.GoogleClientSecret != "" && !strings.HasPrefix(body.GoogleClientSecret, "••") {
		setConfigVal("google_client_secret", body.GoogleClientSecret)
	}
	if body.GoogleRedirectURI != "" {
		setConfigVal("google_redirect_uri", body.GoogleRedirectURI)
	}
	if body.AnthropicAPIKey != "" && !strings.HasPrefix(body.AnthropicAPIKey, "••") {
		setConfigVal("anthropic_api_key", body.AnthropicAPIKey)
	}
	setConfigVal("drive_folder_id", extractFolderID(body.DriveFolderID))
	writeJSON(w, map[string]interface{}{"ok": true, "ready": configReady()})
}

func mask(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", len(s)-4)
}

// --- Static files (embedded into binary) ---

func handleStatic() http.HandlerFunc {
	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("failed to sub frontend fs: %v", err)
	}
	// Read index.html once at startup to serve directly (avoids http.FileServer
	// redirecting /index.html → / which causes an infinite redirect loop).
	indexHTML, err := fs.ReadFile(frontendFS, "frontend/index.html")
	if err != nil {
		log.Fatalf("failed to read embedded index.html: %v", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(indexHTML)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

// --- Main ---

func main() {
	godotenv.Load() // optional: load .env if present (for local dev convenience)
	initDB()
	initSC()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", handleConfigGet)
	mux.HandleFunc("POST /api/config", handleConfigSave)
	mux.HandleFunc("GET /auth/login", handleAuthLogin)
	mux.HandleFunc("GET /auth/callback", handleAuthCallback)
	mux.HandleFunc("GET /auth/status", handleAuthStatus)
	mux.HandleFunc("GET /auth/logout", handleAuthLogout)
	mux.HandleFunc("GET /api/search", handleSearch)
	mux.HandleFunc("POST /api/context", handleContext)
	mux.HandleFunc("GET /api/debug", handleDebug)
	mux.HandleFunc("GET /api/debug/chapter", handleDebugChapter)
	mux.HandleFunc("GET /api/debug/toc", handleDebugTOC)
	mux.HandleFunc("POST /api/recap", handleRecap)
	mux.HandleFunc("POST /api/chat", handleChat)
	mux.HandleFunc("POST /api/photo", handlePhoto)
	mux.HandleFunc("POST /api/photo-recap", handlePhotoRecap)
	mux.HandleFunc("GET /icon-192.png", handleIcon192)
	mux.HandleFunc("GET /icon-512.png", handleIcon512)
	mux.HandleFunc("/", handleStatic())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("BookRecap running on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
