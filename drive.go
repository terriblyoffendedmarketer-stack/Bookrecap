package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	googleoauth2 "google.golang.org/api/oauth2/v2"
)

var driveScopes = []string{
	drive.DriveReadonlyScope,
	"https://www.googleapis.com/auth/userinfo.email",
}

func oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("GOOGLE_REDIRECT_URI"),
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
	result, err := svc.Files.List().
		Q(`(mimeType='application/epub+zip' or mimeType='application/pdf' or name contains '.epub' or name contains '.pdf') and trashed=false`).
		Fields("files(id,name,mimeType)").
		PageSize(200).
		OrderBy("viewedByMeTime desc").
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

	// Score by fuzzy match against query
	q := strings.ToLower(query)
	for i := range files {
		files[i].Score = fuzzyScore(strings.ToLower(files[i].Name), q)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Score > files[j].Score
	})
	// Return only files with some match
	var matched []DriveFile
	for _, f := range files {
		if f.Score > 0 {
			matched = append(matched, f)
		}
	}
	if len(matched) > 10 {
		matched = matched[:10]
	}
	if len(matched) == 0 && len(files) > 0 {
		// No match — return top recent files anyway
		if len(files) > 10 {
			files = files[:10]
		}
		return files, nil
	}
	return matched, nil
}

// fuzzyScore returns how well candidate matches query (higher = better).
// Uses bigram overlap + prefix bonus.
func fuzzyScore(candidate, query string) int {
	if candidate == query {
		return 1000
	}
	if strings.Contains(candidate, query) {
		return 500 + len(query)
	}
	queryBigrams := bigrams(query)
	candidateBigrams := bigrams(candidate)
	shared := 0
	for bg := range queryBigrams {
		if candidateBigrams[bg] {
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
