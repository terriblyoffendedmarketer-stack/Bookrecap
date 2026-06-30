package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/securecookie"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
)

var sc *securecookie.SecureCookie

func init() {
	godotenv.Load()
	hashKey := []byte(padKey(os.Getenv("SECRET_KEY"), 32))
	blockKey := []byte(padKey(os.Getenv("SECRET_BLOCK_KEY"), 32))
	sc = securecookie.New(hashKey, blockKey)
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
	}

	titles := make([]string, len(chapters))
	for i, ch := range chapters {
		titles[i] = ch.Title
	}
	writeJSON(w, map[string]interface{}{
		"chapter_count": len(chapters),
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
	sseHeaders(w)
	flush := flusher(w)
	if err := StreamRecap(w, flush, body.Title, chapters, body.ChapterCount); err != nil {
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
	sseHeaders(w)
	flush := flusher(w)
	if err := StreamChat(w, flush, body.Title, chapters, body.ChapterCount, body.Messages); err != nil {
		log.Printf("chat stream error: %v", err)
	}
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

	upTo := body.ChapterCount
	// Auto-detect chapter position from image
	if upTo <= 0 {
		snippet, err := IdentifyChapterFromImage(body.ImageB64, body.MediaType)
		if err == nil && snippet != "" {
			upTo = findChapterForText(chapters, snippet)
		}
		if upTo <= 0 {
			upTo = len(chapters)
		}
	}

	sseHeaders(w)
	// Send the detected chapter back as first SSE event so UI can display it
	fmt.Fprintf(w, "data: {\"chapter_detected\":%s}\n\n", strconv.Itoa(upTo))
	flusher(w)()

	flush := flusher(w)
	if err := StreamPhoto(w, flush, body.Title, chapters, upTo, body.ImageB64, body.MediaType, body.Question); err != nil {
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

	upTo := len(chapters)
	snippet, err := IdentifyChapterFromImage(body.ImageB64, body.MediaType)
	if err == nil && snippet != "" {
		upTo = findChapterForText(chapters, snippet)
	}

	sseHeaders(w)
	fmt.Fprintf(w, "data: {\"chapter_detected\":%d}\n\n", upTo)
	flusher(w)()

	flush := flusher(w)
	if err := StreamRecap(w, flush, body.Title, chapters, upTo); err != nil {
		log.Printf("photo recap stream error: %v", err)
	}
}

// --- Static files ---

func handleStatic(frontendDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		fp := filepath.Join(frontendDir, filepath.Clean(path))
		// Security: ensure we stay within frontend dir
		if !strings.HasPrefix(fp, frontendDir) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, fp)
	}
}

// --- Main ---

func main() {
	initDB()

	frontendDir, _ := filepath.Abs("frontend")
	if d := os.Getenv("FRONTEND_DIR"); d != "" {
		frontendDir = d
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", handleAuthLogin)
	mux.HandleFunc("GET /auth/callback", handleAuthCallback)
	mux.HandleFunc("GET /auth/status", handleAuthStatus)
	mux.HandleFunc("GET /auth/logout", handleAuthLogout)
	mux.HandleFunc("GET /api/search", handleSearch)
	mux.HandleFunc("POST /api/context", handleContext)
	mux.HandleFunc("POST /api/recap", handleRecap)
	mux.HandleFunc("POST /api/chat", handleChat)
	mux.HandleFunc("POST /api/photo", handlePhoto)
	mux.HandleFunc("POST /api/photo-recap", handlePhotoRecap)
	mux.HandleFunc("/", handleStatic(frontendDir))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("BookRecap running on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

