package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const claudeAPI = "https://api.anthropic.com/v1/messages"
const claudeModel = "claude-sonnet-4-6"

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

func chapterLabel(chapters []Chapter, upTo int) string {
	if upTo >= 1 && upTo <= len(chapters) {
		return chapters[upTo-1].Title
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
func StreamRecap(w io.Writer, flush func(), title string, chapters []Chapter, upTo int) error {
	safe := extractChapters(chapters, upTo)
	label := chapterLabel(chapters, upTo)
	ctx := buildContext(safe, 120_000, 8_000)

	prompt := fmt.Sprintf(`Here is the book text up to %s:

%s

---

Write a "previously on…" recap for someone who put this book down and needs to get back into it. Structure it as:

**The story so far** – what has happened, in order, clearly summarized
**Key characters** – who they are, their roles, how they relate to each other
**What's at stake** – the main conflict and tensions right now
**Where things left off** – exact situation at the end of %s

Plain language, no spoilers past %s.`, label, ctx, label, label)

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
func StreamChat(w io.Writer, flush func(), title string, chapters []Chapter, upTo int, messages []claudeMessage) error {
	safe := extractChapters(chapters, upTo)
	label := chapterLabel(chapters, upTo)
	ctx := buildContext(safe, 100_000, 8_000)

	// Inject book context as a priming exchange so it doesn't eat the system prompt
	primed := []claudeMessage{
		{Role: "user", Content: fmt.Sprintf("Here is the text of \"%s\" up to %s. Use this as your reference:\n\n%s", title, label, ctx)},
		{Role: "assistant", Content: fmt.Sprintf("Got it — I have the full text of \"%s\" up to %s. Ask me anything about what you've read so far.", title, label)},
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
func StreamPhoto(w io.Writer, flush func(), title string, chapters []Chapter, upTo int, imageB64, mediaType, question string) error {
	safe := extractChapters(chapters, upTo)
	label := chapterLabel(chapters, upTo)
	ctx := buildContext(safe, 80_000, 6_000)

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
