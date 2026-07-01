package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

const claudeAPI = "https://api.anthropic.com/v1/messages"
const claudeModel = "claude-sonnet-4-6"
const haikuModel = "claude-haiku-4-5-20251001"

// extractChapters returns only chapters 1..upTo — the hard spoiler gate.
// Nothing past upTo is ever included.
func extractChapters(chapters []Chapter, upTo int) []Chapter {
	if upTo > len(chapters) {
		upTo = len(chapters)
	}
	if upTo < 1 {
		upTo = 1
	}
	return chapters[:upTo]
}

// extractChaptersPartial behaves like extractChapters but additionally truncates
// the text of the final included chapter to the given character offset, so a
// reader caught mid-chapter (e.g. via a photo on page 2 of a 20-page chapter)
// doesn't get spoiled by the rest of that chapter. offset < 0 means no
// truncation — the whole final chapter is treated as read.
func extractChaptersPartial(chapters []Chapter, upTo, offset int) []Chapter {
	safe := extractChapters(chapters, upTo)
	if offset < 0 || len(safe) == 0 {
		return safe
	}
	last := safe[len(safe)-1]
	if offset < len(last.Text) {
		trimmed := make([]Chapter, len(safe))
		copy(trimmed, safe)
		last.Text = last.Text[:offset]
		trimmed[len(trimmed)-1] = last
		return trimmed
	}
	return safe
}

// buildContext concatenates chapter texts up to a total token budget.
// Each chapter is capped at maxChapterChars to prevent any single long
// chapter from eating the whole context.
func buildContext(chapters []Chapter, maxTotalChars, maxChapterChars int) string {
	var parts []string
	total := 0
	for _, ch := range chapters {
		text := ch.Text
		if len(text) > maxChapterChars {
			text = text[:maxChapterChars] + "\n[...chapter continues...]"
		}
		section := fmt.Sprintf("=== %s ===\n%s", ch.Title, text)
		if total+len(section) > maxTotalChars {
			break
		}
		parts = append(parts, section)
		total += len(section)
	}
	return strings.Join(parts, "\n\n")
}

// buildContextSmart uses pre-generated summaries for older chapters and full
// text for the most-recent fullTextWindow chapters. This lets Claude understand
// the entire story arc even for very long books without overflowing the context
// budget. When summaries is nil or empty, it falls back to buildContext.
func buildContextSmart(summaries []string, chapters []Chapter, fullTextWindow, maxChapterChars int) string {
	if len(summaries) == 0 {
		return buildContext(chapters, 100_000, maxChapterChars)
	}
	splitAt := len(chapters) - fullTextWindow
	if splitAt < 0 {
		splitAt = 0
	}
	var parts []string
	for i := 0; i < splitAt; i++ {
		if i < len(summaries) && summaries[i] != "" {
			parts = append(parts, fmt.Sprintf("=== %s [summary] ===\n%s", chapters[i].Title, summaries[i]))
		}
	}
	for i := splitAt; i < len(chapters); i++ {
		text := chapters[i].Text
		if len(text) > maxChapterChars {
			text = text[:maxChapterChars] + "\n[...chapter continues...]"
		}
		parts = append(parts, fmt.Sprintf("=== %s ===\n%s", chapters[i].Title, text))
	}
	return strings.Join(parts, "\n\n")
}

// summarizeChapter calls Haiku to produce a 2-3 sentence chapter summary.
func summarizeChapter(ch Chapter) (string, error) {
	text := ch.Text
	if len(text) > 5000 {
		// Include both start and end of chapter to capture setup + resolution.
		text = text[:2500] + "\n[...]\n" + text[len(text)-2500:]
	}
	prompt := fmt.Sprintf(`Summarize this book chapter in 2-3 sentences. Be specific: name the key events, characters involved, and any important decisions or revelations. No vague filler.

Chapter: %s

Text:
%s`, ch.Title, text)

	req := claudeRequest{
		Model:     haikuModel,
		MaxTokens: 150,
		Messages:  []claudeMessage{{Role: "user", Content: prompt}},
		Stream:    false,
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", claudeAPI, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", loadConfig().AnthropicAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Content) > 0 && result.Content[0].Text != "" {
		return strings.TrimSpace(result.Content[0].Text), nil
	}
	return "", fmt.Errorf("no summary returned for %s", ch.Title)
}

// SummarizeChapters generates a Haiku summary for every chapter in parallel
// (up to 8 concurrent requests). Errors fall back to the chapter title so the
// returned slice always has the same length as chapters.
func SummarizeChapters(chapters []Chapter) []string {
	summaries := make([]string, len(chapters))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, ch := range chapters {
		wg.Add(1)
		go func(idx int, chapter Chapter) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := summarizeChapter(chapter)
			if err != nil {
				log.Printf("summary failed ch%d (%s): %v", idx+1, chapter.Title, err)
				summaries[idx] = chapter.Title
			} else {
				summaries[idx] = s
			}
		}(i, ch)
	}
	wg.Wait()
	return summaries
}

func chapterLabel(chapters []Chapter, upTo int) string {
	if upTo >= 1 && upTo <= len(chapters) {
		title := chapters[upTo-1].Title
		if title == "" {
			return fmt.Sprintf("Chapter %d", upTo)
		}
		return fmt.Sprintf("Chapter %d (%s)", upTo, title)
	}
	return fmt.Sprintf("Chapter %d", upTo)
}

func systemPrompt(title, label string) string {
	return fmt.Sprintf(`You are a reading assistant for "%s". The reader has read up to %s.

SPOILER GATE — NON-NEGOTIABLE:
You only have access to the text up to %s. You MUST NOT reveal, hint at, foreshadow, or reference anything that happens after this point. If asked about events past this point, say you don't know yet.

Tone: plain, casual, everyday English. You are talking to someone who is not an experienced reader and finds dense prose confusing.
- Unpack jargon, sci-fi terms, and world-building concepts clearly.
- Trace chains of events step-by-step when needed.
- Be concrete about who characters are and how they relate.
- Keep answers easy to skim on a phone screen.`, title, label, label)
}

// --- Claude API streaming ---

type claudeMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system"`
	Messages  []claudeMessage `json:"messages"`
	Stream    bool            `json:"stream"`
}

type imageContent struct {
	Type   string      `json:"type"`
	Source imageSource `json:"source"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func streamClaude(w io.Writer, flush func(), req claudeRequest) error {
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", claudeAPI, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", loadConfig().AnthropicAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("claude API error %d: %s", resp.StatusCode, string(b))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		if evt["type"] == "content_block_delta" {
			if delta, ok := evt["delta"].(map[string]interface{}); ok {
				if text, ok := delta["text"].(string); ok {
					fmt.Fprintf(w, "data: %s\n\n", jsonStr(text))
					flush()
				}
			}
		}
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flush()
	return scanner.Err()
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// StreamRecap sends a "previously on" recap up to chapter upTo.
// fromChapter sets the start of the window (1 = beginning). Values <= 1
// include all chapters up to upTo.
// summaries is the pre-generated per-chapter summary cache; may be nil.
func StreamRecap(w io.Writer, flush func(), title string, chapters []Chapter, summaries []string, upTo, fromChapter int) error {
	allSafe := extractChapters(chapters, upTo)
	label := chapterLabel(chapters, upTo)

	// Enforce spoiler gate on summaries.
	safeSummaries := summaries
	if len(safeSummaries) > upTo {
		safeSummaries = safeSummaries[:upTo]
	}

	var ctx, ctxHeader string
	if fromChapter > 1 {
		// Windowed recap: summaries for pre-window backstory + full text for window.
		fullTextWindow := upTo - fromChapter + 1
		if fullTextWindow < 1 {
			fullTextWindow = 1
		}
		fromLabel := chapterLabel(chapters, fromChapter)
		if len(safeSummaries) > 0 {
			ctx = buildContextSmart(safeSummaries, allSafe, fullTextWindow, 8_000)
			ctxHeader = fmt.Sprintf(`Here is the content of "%s" covering %s through %s (chapters before %s are provided as brief summaries for background):`, title, fromLabel, label, fromLabel)
		} else {
			windowChapters := allSafe
			if fromChapter-1 < len(windowChapters) {
				windowChapters = windowChapters[fromChapter-1:]
			}
			ctx = buildContext(windowChapters, 120_000, 8_000)
			ctxHeader = fmt.Sprintf(`Here is the text of "%s" covering %s through %s:`, title, fromLabel, label)
		}

		prompt := fmt.Sprintf(`%s

%s

---

Write a focused "previously on…" for someone jumping back into this part of the book. Cover %s through %s only.

Structure:
**What happened** – the key events in this section, in plain English
**Why it matters** – bring in any earlier thread only when it directly explains something here; skip everything else
**Key players right now** – who is involved and what each one wants at this moment
**Where things stand** – the exact situation and open tension at the end of %s

Lead with the most recent events, not the earliest. No spoilers past %s.`,
			ctxHeader, ctx, fromLabel, label, label, label)

		return streamClaude(w, flush, claudeRequest{
			Model:     claudeModel,
			MaxTokens: 2000,
			System:    systemPrompt(title, label),
			Messages:  []claudeMessage{{Role: "user", Content: prompt}},
			Stream:    true,
		})
	}

	// Full-history recap: summaries for older chapters + full text for recent ones.
	if len(safeSummaries) > 0 {
		fullTextWindow := min(10, len(allSafe))
		ctx = buildContextSmart(safeSummaries, allSafe, fullTextWindow, 8_000)
		ctxHeader = fmt.Sprintf(`Here is the full content of "%s" up through %s (older chapters as brief summaries, recent chapters as full text):`, title, label)
	} else {
		ctx = buildContext(allSafe, 120_000, 8_000)
		ctxHeader = fmt.Sprintf(`Here is the full text of "%s" up through %s:`, title, label)
	}

	prompt := fmt.Sprintf(`%s

%s

---

Write a "previously on…" for someone who put this book down and needs to get back into it. You are writing the "previously on" segment that plays before a new episode of a TV show — it covers what the viewer needs to know RIGHT NOW, not everything that has ever happened.

Structure:
**What just happened** – lead with the most recent key events, explained clearly
**The bigger picture** – bring in earlier events ONLY when they directly explain what is happening right now; skip anything that does not connect to the current situation
**Key players right now** – who matters at this moment and what they each want
**Where things stand** – the exact situation and tension at the end of %s

Do NOT do a chapter-by-chapter chronological walkthrough. Do NOT mention characters or events from early in the book unless they are directly relevant to the current situation. Be specific, not vague. Plain language, no spoilers past %s.`,
		ctxHeader, ctx, label, label)

	return streamClaude(w, flush, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 2000,
		System:    systemPrompt(title, label),
		Messages:  []claudeMessage{{Role: "user", Content: prompt}},
		Stream:    true,
	})
}

// StreamChat handles a multi-turn conversation about the book.
// messages is the conversation history (without book context injected).
// summaries is the pre-generated per-chapter summary cache; may be nil.
func StreamChat(w io.Writer, flush func(), title string, chapters []Chapter, summaries []string, upTo int, messages []claudeMessage) error {
	safe := extractChapters(chapters, upTo)
	label := chapterLabel(chapters, upTo)

	safeSummaries := summaries
	if len(safeSummaries) > upTo {
		safeSummaries = safeSummaries[:upTo]
	}

	var ctx string
	if len(safeSummaries) > 0 {
		// Last 10 chapters as full text; everything before as summaries.
		ctx = buildContextSmart(safeSummaries, safe, min(10, len(safe)), 8_000)
	} else {
		ctx = buildContext(safe, 100_000, 8_000)
	}

	// Inject book context as a priming exchange so it doesn't eat the system prompt.
	primed := []claudeMessage{
		{Role: "user", Content: fmt.Sprintf("Here is the content of \"%s\" covering all %d chapters through %s. Use this as your reference:\n\n%s", title, len(safe), label, ctx)},
		{Role: "assistant", Content: fmt.Sprintf("Got it — I have the complete content of \"%s\" for all %d chapters through %s. I can answer questions about any events, characters, or details from the entire reading so far. Ask me anything.", title, len(safe), label)},
	}
	primed = append(primed, messages...)

	return streamClaude(w, flush, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1000,
		System:    systemPrompt(title, label),
		Messages:  primed,
		Stream:    true,
	})
}

// IdentifyChapterFromImage asks Claude to extract the visible text from a photo,
// then we search for that text in the cached chapters to find the position.
// Returns the extracted text snippet (caller does the chapter search).
func IdentifyChapterFromImage(imageB64, mediaType string) (string, error) {
	content := []interface{}{
		textContent{Type: "text", Text: "Extract the text visible in this image verbatim. Output ONLY the text you can read — no commentary, no formatting. If it's a Kindle or book page, give me the first 3-4 sentences you can see."},
		imageContent{
			Type: "image",
			Source: imageSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      imageB64,
			},
		},
	}

	req := claudeRequest{
		Model:     claudeModel,
		MaxTokens: 300,
		Messages:  []claudeMessage{{Role: "user", Content: content}},
		Stream:    false,
	}

	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", claudeAPI, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", loadConfig().AnthropicAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Content) > 0 {
		return result.Content[0].Text, nil
	}
	return "", fmt.Errorf("no text extracted from image")
}

// StreamPhoto answers a question about a book page photo.
// upTo is already resolved to the correct chapter by the caller.
// summaries is the pre-generated per-chapter summary cache; may be nil.
func StreamPhoto(w io.Writer, flush func(), title string, chapters []Chapter, summaries []string, upTo int, imageB64, mediaType, question string) error {
	safe := extractChapters(chapters, upTo)
	label := chapterLabel(chapters, upTo)

	safeSummaries := summaries
	if len(safeSummaries) > upTo {
		safeSummaries = safeSummaries[:upTo]
	}

	var ctx string
	if len(safeSummaries) > 0 {
		ctx = buildContextSmart(safeSummaries, safe, min(5, len(safe)), 6_000)
	} else {
		ctx = buildContext(safe, 80_000, 6_000)
	}

	content := []interface{}{
		textContent{
			Type: "text",
			Text: fmt.Sprintf(`Book context — "%s" up to %s:

%s

---

The reader is showing you a photo of a page they're currently reading. Look at the image and answer their question in plain English, using the book context above.

Reader's question: %s`, title, label, ctx, question),
		},
		imageContent{
			Type: "image",
			Source: imageSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      imageB64,
			},
		},
	}

	return streamClaude(w, flush, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1200,
		System:    systemPrompt(title, label),
		Messages:  []claudeMessage{{Role: "user", Content: content}},
		Stream:    true,
	})
}
