package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Chapter struct {
	Index  int    `json:"index"`
	Number int    `json:"number"` // the book's own printed chapter number, when its heading embeds one; 0 for unnumbered front/back matter
	Href   string `json:"href"`   // epub-root-relative source file path (empty for non-epub books), used to match against the TOC
	Title  string `json:"title"`
	Text   string `json:"text"`
}

var db *sql.DB

func initDB() {
	dbPath := filepath.Join(os.Getenv("DATA_DIR"), "cache.db")
	if os.Getenv("DATA_DIR") == "" {
		dbPath = "cache.db"
	}
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("failed to open sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS books (
			file_id   TEXT PRIMARY KEY,
			title     TEXT NOT NULL,
			chapters  TEXT NOT NULL,
			summaries TEXT,
			cached_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS config (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		log.Fatalf("failed to create tables: %v", err)
	}
	// Add summaries column to existing databases that predate it (error is normal and safe to ignore).
	db.Exec(`ALTER TABLE books ADD COLUMN summaries TEXT`)
	// Temporary: cache the raw epub bytes so the actual EPUB nav/TOC
	// structure can be inspected via a debug endpoint without needing
	// separate Google Drive access. Diagnostic only — safe to drop later.
	db.Exec(`ALTER TABLE books ADD COLUMN raw_epub BLOB`)
}

func getConfigVal(key string) string {
	row := db.QueryRow("SELECT value FROM config WHERE key = ?", key)
	var val string
	if err := row.Scan(&val); err != nil {
		return ""
	}
	return val
}

func setConfigVal(key, value string) {
	_, err := db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)", key, value)
	if err != nil {
		log.Printf("config write error: %v", err)
	}
}

// latestBook returns the file_id and title of the most recently cached
// book, so the debug endpoint can be used without knowing the file_id.
func latestBook() (fileID, title string, ok bool) {
	row := db.QueryRow("SELECT file_id, title FROM books ORDER BY cached_at DESC LIMIT 1")
	if err := row.Scan(&fileID, &title); err != nil {
		return "", "", false
	}
	return fileID, title, true
}

func getChapters(fileID string) ([]Chapter, bool) {
	row := db.QueryRow("SELECT chapters FROM books WHERE file_id = ?", fileID)
	var raw string
	if err := row.Scan(&raw); err != nil {
		return nil, false
	}
	var chapters []Chapter
	if err := json.Unmarshal([]byte(raw), &chapters); err != nil {
		return nil, false
	}
	return chapters, true
}

func setChapters(fileID, title string, chapters []Chapter) {
	raw, _ := json.Marshal(chapters)
	_, err := db.Exec(`
		INSERT OR REPLACE INTO books (file_id, title, chapters, cached_at)
		VALUES (?, ?, ?, ?)
	`, fileID, title, string(raw), time.Now().Unix())
	if err != nil {
		log.Printf("cache write error: %v", err)
	}
}

// setRawEpub caches the original epub bytes for a book, temporarily, so the
// real EPUB nav/TOC structure can be inspected directly for diagnostics.
func setRawEpub(fileID string, data []byte) {
	_, err := db.Exec("UPDATE books SET raw_epub = ? WHERE file_id = ?", data, fileID)
	if err != nil {
		log.Printf("raw epub write error: %v", err)
	}
}

func getRawEpub(fileID string) ([]byte, bool) {
	row := db.QueryRow("SELECT raw_epub FROM books WHERE file_id = ?", fileID)
	var raw []byte
	if err := row.Scan(&raw); err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

func getSummaries(fileID string) ([]string, bool) {
	row := db.QueryRow("SELECT summaries FROM books WHERE file_id = ?", fileID)
	var raw sql.NullString
	if err := row.Scan(&raw); err != nil || !raw.Valid || raw.String == "" {
		return nil, false
	}
	var summaries []string
	if err := json.Unmarshal([]byte(raw.String), &summaries); err != nil {
		return nil, false
	}
	return summaries, len(summaries) > 0
}

func setSummaries(fileID string, summaries []string) {
	raw, _ := json.Marshal(summaries)
	_, err := db.Exec("UPDATE books SET summaries = ? WHERE file_id = ?", string(raw), fileID)
	if err != nil {
		log.Printf("summaries write error: %v", err)
	}
}
