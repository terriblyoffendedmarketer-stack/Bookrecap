package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	googleoauth2 "google.golang.org/api/oauth2/v2"
)

var folderURLRe = regexp.MustCompile(`/folders/([a-zA-Z0-9_-]+)`)

// extractFolderID returns the folder ID from a Drive URL or the raw string if it's already an ID.
func extractFolderID(s string) string {
	s = strings.TrimSpace(s)
	if m := folderURLRe.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return s
}

var driveScopes = []string{
	drive.DriveReadonlyScope,
	"https://www.googleapis.com/auth/userinfo.email",
}

func oauthConfig() *oauth2.Config {
	cfg := loadConfig()
	return &oauth2.Config{
		ClientID:     cfg.GoogleClientID,
		ClientSecret: cfg.GoogleClientSecret,
		RedirectURL:  cfg.GoogleRedirectURI,
		Scopes:       driveScopes,
		Endpoint:     google.Endpoint,
	}
}

func getAuthURL(state string) string {
	return oauthConfig().AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
}

func exchangeCode(code string) (*oauth2.Token, error) {
	return oauthConfig().Exchange(context.Background(), code)
}

func tokenToMap(t *oauth2.Token) map[string]interface{} {
	return map[string]interface{}{
		"access_token":  t.AccessToken,
		"refresh_token": t.RefreshToken,
		"token_type":    t.TokenType,
		"expiry":        t.Expiry.Unix(),
	}
}

func driveService(token *oauth2.Token) (*drive.Service, error) {
	cfg := oauthConfig()
	ts := cfg.TokenSource(context.Background(), token)
	return drive.NewService(context.Background(), option.WithTokenSource(ts))
}

type DriveFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Score    int    `json:"score"`
}

func searchBooks(token *oauth2.Token, query string) ([]DriveFile, error) {
	svc, err := driveService(token)
	if err != nil {
		return nil, err
	}
	cfg := loadConfig()

	fileFilter := `(mimeType='application/epub+zip' or mimeType='application/pdf' or name contains '.epub' or name contains '.pdf') and trashed=false`
	driveQ := fileFilter

	// Use Drive's own index to narrow results by query words — avoids the
	// "only first 200 files" blind spot when the library is large.
	if query != "" {
		if nf := buildNameFilter(query); nf != "" {
			driveQ += " and " + nf
		}
	}

	// Folder filter: recursively collect all subfolder IDs so nested books appear.
	if cfg.DriveFolderID != "" {
		folderIDs := collectFolderIDs(svc, cfg.DriveFolderID)
		var parentClauses []string
		for _, fid := range folderIDs {
			parentClauses = append(parentClauses, fmt.Sprintf("'%s' in parents", fid))
		}
		driveQ += " and (" + strings.Join(parentClauses, " or ") + ")"
	}

	result, err := svc.Files.List().
		Q(driveQ).
		Fields("files(id,name,mimeType)").
		PageSize(100).
		OrderBy("viewedByMeTime desc").
		IncludeItemsFromAllDrives(true).
		SupportsAllDrives(true).
		Corpora("allDrives").
		Do()
	if err != nil {
		return nil, fmt.Errorf("drive list: %w", err)
	}

	var files []DriveFile
	for _, f := range result.Files {
		files = append(files, DriveFile{ID: f.Id, Name: f.Name, MimeType: f.MimeType})
	}

	if query == "" {
		if len(files) > 20 {
			return files[:20], nil
		}
		return files, nil
	}

	q := strings.ToLower(query)
	for i := range files {
		files[i].Score = fuzzyScore(strings.ToLower(files[i].Name), q)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Score > files[j].Score
	})
	if len(files) > 10 {
		files = files[:10]
	}
	return files, nil
}

// collectFolderIDs returns the root folder ID plus all descendant folder IDs
// (BFS, capped at 40 folders to keep query length reasonable).
func collectFolderIDs(svc *drive.Service, rootID string) []string {
	ids := []string{rootID}
	queue := []string{rootID}
	for len(queue) > 0 && len(ids) < 40 {
		cur := queue[0]
		queue = queue[1:]
		res, err := svc.Files.List().
			Q(fmt.Sprintf("'%s' in parents and mimeType='application/vnd.google-apps.folder' and trashed=false", cur)).
			Fields("files(id)").
			PageSize(100).
			IncludeItemsFromAllDrives(true).
			SupportsAllDrives(true).
			Do()
		if err != nil {
			break
		}
		for _, f := range res.Files {
			ids = append(ids, f.Id)
			queue = append(queue, f.Id)
		}
	}
	return ids
}

// buildNameFilter builds a Drive query clause that matches files whose name
// contains any of the meaningful words in the user's query.
func buildNameFilter(query string) string {
	words := strings.Fields(strings.ToLower(query))
	var chosen []string
	for _, w := range words {
		if len(w) >= 4 {
			chosen = append(chosen, w)
		}
	}
	if len(chosen) == 0 {
		for _, w := range words {
			if len(w) >= 3 {
				chosen = append(chosen, w)
			}
		}
	}
	if len(chosen) == 0 {
		return ""
	}
	var clauses []string
	for _, w := range chosen {
		w = strings.ReplaceAll(w, "'", `\'`)
		clauses = append(clauses, fmt.Sprintf("name contains '%s'", w))
	}
	return "(" + strings.Join(clauses, " or ") + ")"
}

// fuzzyScore returns how well candidate matches query (higher = better).
func fuzzyScore(candidate, query string) int {
	if query == "" {
		return 1
	}
	if candidate == query {
		return 1000
	}
	if strings.HasPrefix(candidate, query) {
		return 900
	}
	if strings.Contains(candidate, query) {
		return 500 + len(query)
	}
	// Word-level prefix matching
	score := 0
	for _, qw := range strings.Fields(query) {
		if len(qw) < 2 {
			continue
		}
		for _, cw := range strings.Fields(candidate) {
			if strings.HasPrefix(cw, qw) {
				score += 100 + len(qw)*5
			} else if strings.Contains(cw, qw) {
				score += 40 + len(qw)*2
			}
		}
	}
	if score > 0 {
		return score
	}
	// Bigram fallback
	qBigrams := bigrams(query)
	cBigrams := bigrams(candidate)
	shared := 0
	for bg := range qBigrams {
		if cBigrams[bg] {
			shared++
		}
	}
	return shared
}

func bigrams(s string) map[string]bool {
	m := make(map[string]bool)
	r := []rune(s)
	for i := 0; i < len(r)-1; i++ {
		m[string(r[i:i+2])] = true
	}
	return m
}

func downloadFile(token *oauth2.Token, fileID string) ([]byte, string, error) {
	svc, err := driveService(token)
	if err != nil {
		return nil, "", err
	}
	meta, err := svc.Files.Get(fileID).Fields("mimeType", "name").Do()
	if err != nil {
		return nil, "", fmt.Errorf("get file meta: %w", err)
	}
	resp, err := svc.Files.Get(fileID).Download()
	if err != nil {
		return nil, "", fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return data, meta.MimeType, nil
}

func getUserEmail(token *oauth2.Token) string {
	cfg := oauthConfig()
	ts := cfg.TokenSource(context.Background(), token)
	svc, err := googleoauth2.NewService(context.Background(), option.WithTokenSource(ts))
	if err != nil {
		return ""
	}
	info, err := svc.Userinfo.Get().Do()
	if err != nil {
		return ""
	}
	return info.Email
}
