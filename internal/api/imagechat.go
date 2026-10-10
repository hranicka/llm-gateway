package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"llm-gateway/internal/manager"
)

// Image-chat bridge: chat clients (e.g. Open WebUI's model picker) can use an
// sd-server model flagged image_chat as a chat model. The last user message
// becomes the prompt for /v1/images/generations — or /v1/images/edits when it
// carries images — and the assistant reply is the image as a markdown data
// URL. Only that last message is read, so earlier replies never feed back.

// keepAliveInterval spaces SSE comments while an image renders.
const keepAliveInterval = 15 * time.Second

// streamChunkBytes bounds each SSE content delta: a rendered image is several
// megabytes of base64, and clients read streams line by line with a limited
// buffer. Deltas are concatenated by the client.
const streamChunkBytes = 32 << 10

// taskPromptPrefix starts every Open WebUI background task prompt (title,
// tags, follow-up suggestions). Those go to the selected model, so for an
// image model they are answered with a stub instead of burning a generation.
const taskPromptPrefix = "### Task:"

// taskStub satisfies the JSON shapes Open WebUI parses for title, tags and
// follow-up tasks.
const taskStub = `{"title": "Image", "tags": [], "follow_ups": []}`

type imageChatRequest struct {
	Stream   bool `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type imageResponse struct {
	Data []struct {
		B64 string `json:"b64_json"`
	} `json:"data"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// lastUserInput returns the text and the decoded data-URL images of the
// last user message. Remote image URLs are ignored.
func lastUserInput(req imageChatRequest) (string, [][]byte) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		raw := req.Messages[i].Content
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return strings.TrimSpace(text), nil
		}
		var parts []contentPart
		if err := json.Unmarshal(raw, &parts); err != nil {
			return "", nil
		}
		var texts []string
		var images [][]byte
		for _, p := range parts {
			switch p.Type {
			case "text":
				texts = append(texts, p.Text)
			case "image_url":
				if img := decodeDataURL(p.ImageURL.URL); img != nil {
					images = append(images, img)
				}
			}
		}
		return strings.TrimSpace(strings.Join(texts, "\n")), images
	}
	return "", nil
}

func decodeDataURL(u string) []byte {
	if !strings.HasPrefix(u, "data:") {
		return nil
	}
	_, payload, ok := strings.Cut(u, ",")
	if !ok {
		return nil
	}
	img, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	return img
}

// imageMIME names the image format from its base64 prefix (PNG is sd-server's
// default output).
func imageMIME(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	default:
		return "image/png"
	}
}

// requestImage asks the sd-server backend for one image and returns it as
// base64. Edits (multipart) are used when reference images are present.
func requestImage(ctx context.Context, backend, model, prompt string, images [][]byte) (string, error) {
	var (
		endpoint    = "/v1/images/generations"
		contentType = "application/json"
		body        bytes.Buffer
	)
	if len(images) == 0 {
		_ = json.NewEncoder(&body).Encode(map[string]any{
			"model": model, "prompt": prompt, "n": 1, "response_format": "b64_json",
		})
	} else {
		endpoint = "/v1/images/edits"
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("model", model)
		_ = mw.WriteField("prompt", prompt)
		_ = mw.WriteField("n", "1")
		_ = mw.WriteField("response_format", "b64_json")
		for i, img := range images {
			fw, err := mw.CreateFormFile("image[]", fmt.Sprintf("image-%d.png", i))
			if err != nil {
				return "", err
			}
			if _, err := fw.Write(img); err != nil {
				return "", err
			}
		}
		if err := mw.Close(); err != nil {
			return "", err
		}
		contentType = mw.FormDataContentType()
	}

	target, err := url.JoinPath(backend, endpoint)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(loopDetectHeader, "1")

	// No client timeout: a render can legitimately take minutes; the request
	// context ends it when the chat client goes away.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return "", err
	}
	var out imageResponse
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data[:min(len(data), 200)]))
		}
		return "", fmt.Errorf("image backend returned %d: %s", resp.StatusCode, msg)
	}
	if len(out.Data) == 0 || out.Data[0].B64 == "" {
		return "", fmt.Errorf("image backend returned no image")
	}
	return out.Data[0].B64, nil
}

// imageChatHandler answers a chat completion for an image_chat model.
func imageChatHandler(w http.ResponseWriter, r *http.Request, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, "Failed to read request body", "invalid_body", http.StatusBadRequest)
		return
	}
	var req imageChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, "Invalid request body", "invalid_body", http.StatusBadRequest)
		return
	}
	prompt, images := lastUserInput(req)
	if prompt == "" {
		writeOpenAIError(w, "Image models need a user message describing the image", "prompt_required", http.StatusBadRequest)
		return
	}

	out := newChatWriter(w, name, req.Stream)
	if strings.HasPrefix(prompt, taskPromptPrefix) {
		out.begin()
		out.finish(taskStub)
		return
	}

	select {
	case proxySlots <- struct{}{}:
		defer func() { <-proxySlots }()
	case <-r.Context().Done():
		return
	}
	backend, release, err := manager.SwitchModel(name)
	if err != nil {
		slog.Error("switch model failed", "model", name, "error", err)
		writeOpenAIError(w, fmt.Sprintf("Failed to load model %s — check the gateway logs", name), "model_load_failed", http.StatusInternalServerError)
		return
	}
	defer release()

	out.begin()
	type result struct {
		b64 string
		err error
	}
	done := make(chan result, 1)
	go func() {
		b64, err := requestImage(r.Context(), backend, name, prompt, images)
		done <- result{b64, err}
	}()

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case res := <-done:
			if res.err != nil {
				slog.Error("image chat generation failed", "model", name, "error", res.err)
				out.fail(res.err)
				return
			}
			out.finish(fmt.Sprintf("![Generated image](data:%s;base64,%s)", imageMIME(res.b64), res.b64))
			return
		case <-ticker.C:
			out.keepAlive()
		case <-r.Context().Done():
			return
		}
	}
}

// chatWriter emits one assistant message as a chat completion, streamed
// (SSE) or as a single JSON body.
type chatWriter struct {
	w       http.ResponseWriter
	model   string
	id      string
	created int64
	stream  bool
}

func newChatWriter(w http.ResponseWriter, model string, stream bool) *chatWriter {
	now := time.Now()
	return &chatWriter{
		w: w, model: model, stream: stream,
		id:      fmt.Sprintf("chatcmpl-img-%d", now.UnixNano()),
		created: now.Unix(),
	}
}

func (c *chatWriter) chunk(delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	fmt.Fprintf(c.w, "data: %s\n\n", b)
	c.flush()
}

func (c *chatWriter) flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// begin opens the SSE stream with the assistant role so the client shows
// progress while the image renders.
func (c *chatWriter) begin() {
	if !c.stream {
		return
	}
	c.w.Header().Set("Content-Type", "text/event-stream")
	c.w.Header().Set("Cache-Control", "no-cache")
	c.w.WriteHeader(http.StatusOK)
	c.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
}

func (c *chatWriter) keepAlive() {
	if c.stream {
		fmt.Fprint(c.w, ": generating\n\n")
		c.flush()
	}
}

func (c *chatWriter) finish(content string) {
	if c.stream {
		for len(content) > 0 {
			n := min(len(content), streamChunkBytes)
			for n < len(content) && n > 0 && !utf8.RuneStart(content[n]) {
				n--
			}
			c.chunk(map[string]any{"content": content[:n]}, nil)
			content = content[n:]
		}
		c.chunk(map[string]any{}, "stop")
		fmt.Fprint(c.w, "data: [DONE]\n\n")
		c.flush()
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{
		"id": c.id, "object": "chat.completion", "created": c.created, "model": c.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

// fail reports a generation error: as a message in the stream (the status is
// already sent) or as an API error.
func (c *chatWriter) fail(err error) {
	if c.stream {
		c.finish("Image generation failed: " + err.Error())
		return
	}
	writeOpenAIError(c.w, "Image generation failed: "+err.Error(), "image_generation_failed", http.StatusBadGateway)
}
