package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/manager"
)

const testPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// imageBackend stands in for sd-server and records what the bridge sent.
type imageBackend struct {
	srv        *httptest.Server
	calls      atomic.Int32
	genPrompt  atomic.Value // string
	editPrompt atomic.Value // string
	editFiles  atomic.Int32
	status     int
	image      string // base64 returned; testPNGB64 when empty
}

func newImageBackend(t *testing.T) *imageBackend {
	t.Helper()
	b := &imageBackend{status: http.StatusOK}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/images/generations":
			b.calls.Add(1)
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			b.genPrompt.Store(in["prompt"])
			b.respond(w)
		case "/v1/images/edits":
			b.calls.Add(1)
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(p)
				switch p.FormName() {
				case "prompt":
					b.editPrompt.Store(string(data))
				case "image[]":
					b.editFiles.Add(1)
				}
			}
			b.respond(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *imageBackend) respond(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if b.status != http.StatusOK {
		w.WriteHeader(b.status)
		_, _ = io.WriteString(w, `{"error":{"message":"out of memory"}}`)
		return
	}
	image := b.image
	if image == "" {
		image = testPNGB64
	}
	_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+image+`"}]}`)
}

func setImageChatConfig(b *imageBackend) {
	config.ConfigApp = &config.Config{
		Host: "127.0.0.1:9999", AutoUnload: "1h", DrainTimeout: "5s",
		Models: map[string]config.ModelConf{
			"img":  {Kind: config.KindWeb, ImageChat: true, Command: "sleep 60", Host: b.srv.Listener.Addr().String(), ReadyTimeout: "5s"},
			"app":  {Kind: config.KindWeb, Command: "sleep 60", Host: "127.0.0.1:1", ReadyTimeout: "5s"},
			"chat": {Command: "sleep 60", Host: "127.0.0.1:1", ReadyTimeout: "5s"},
		},
	}
	config.SortedModelNames = []string{"app", "chat", "img"}
	manager.Shutdown(context.Background())
}

func chatRequest(path string, payload map[string]any) *http.Request {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestImageChat_NonStreaming(t *testing.T) {
	b := newImageBackend(t)
	setImageChatConfig(b)
	defer manager.ShutdownCurrentModel()

	req := chatRequest("/v1/chat/completions", map[string]any{
		"model": "img",
		"messages": []map[string]any{
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "an old one"},
			{"role": "assistant", "content": "![Generated image](data:image/png;base64,AAAA)"},
			{"role": "user", "content": "a red fox in the snow"},
		},
	})
	w := httptest.NewRecorder()
	RootHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role, Content string
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected completion: %s", w.Body.String())
	}
	want := "![Generated image](data:image/png;base64," + testPNGB64 + ")"
	if got := resp.Choices[0].Message.Content; got != want {
		t.Errorf("content = %q, want the image as a markdown data URL", got)
	}
	if got := b.genPrompt.Load(); got != "a red fox in the snow" {
		t.Errorf("backend prompt = %v, want only the last user message", got)
	}
}

func TestImageChat_Streaming(t *testing.T) {
	b := newImageBackend(t)
	setImageChatConfig(b)
	defer manager.ShutdownCurrentModel()

	req := chatRequest("/v1/chat/completions", map[string]any{
		"model": "img", "stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "a lighthouse at dusk"}}},
		},
	})
	w := httptest.NewRecorder()
	RootHandler(w, req)

	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	out := w.Body.String()
	for _, want := range []string{`"role":"assistant"`, "data:image/png;base64," + testPNGB64, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	if got := b.genPrompt.Load(); got != "a lighthouse at dusk" {
		t.Errorf("backend prompt = %v", got)
	}
}

func TestImageChat_AttachedImageUsesEdits(t *testing.T) {
	b := newImageBackend(t)
	setImageChatConfig(b)
	defer manager.ShutdownCurrentModel()

	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("fakepng"))
	req := chatRequest("/v1/chat/completions", map[string]any{
		"model": "img",
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "make it blue"},
			{"type": "image_url", "image_url": map[string]string{"url": data}},
		}}},
	})
	w := httptest.NewRecorder()
	RootHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if b.editFiles.Load() != 1 || b.editPrompt.Load() != "make it blue" {
		t.Errorf("edit saw prompt %v with %d images, want %q with 1", b.editPrompt.Load(), b.editFiles.Load(), "make it blue")
	}
	if b.genPrompt.Load() != nil {
		t.Error("generation endpoint called for an edit request")
	}
}

func TestImageChat_TaskPromptsAreStubbed(t *testing.T) {
	b := newImageBackend(t)
	setImageChatConfig(b)
	defer manager.ShutdownCurrentModel()

	req := chatRequest("/v1/chat/completions", map[string]any{
		"model": "img",
		"messages": []map[string]any{
			{"role": "user", "content": "### Task:\nGenerate a concise, 3-5 word title with an emoji summarizing the chat history."},
		},
	})
	w := httptest.NewRecorder()
	RootHandler(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `\"title\": \"Image\"`) {
		t.Fatalf("task prompt answer = %d %s, want the JSON stub", w.Code, w.Body.String())
	}
	if b.calls.Load() != 0 {
		t.Errorf("backend called %d times for a task prompt, want 0", b.calls.Load())
	}
}

func TestImageChat_BackendErrorSurfaces(t *testing.T) {
	b := newImageBackend(t)
	b.status = http.StatusInternalServerError
	setImageChatConfig(b)
	defer manager.ShutdownCurrentModel()

	w := httptest.NewRecorder()
	RootHandler(w, chatRequest("/v1/chat/completions", map[string]any{
		"model": "img", "messages": []map[string]any{{"role": "user", "content": "a cat"}},
	}))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", w.Code, w.Body.String())
	}
	assertOpenAICode(t, w, "image_generation_failed")
	if !strings.Contains(w.Body.String(), "out of memory") {
		t.Errorf("error body %q does not carry the backend message", w.Body.String())
	}
}

func TestImageChat_ListingAndOtherEndpoints(t *testing.T) {
	b := newImageBackend(t)
	setImageChatConfig(b)

	w := httptest.NewRecorder()
	ModelsHandler(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	listing := w.Body.String()
	if !strings.Contains(listing, `"id":"img"`) || strings.Contains(listing, `"id":"app"`) {
		t.Errorf("models = %s, want the image_chat model listed and the plain web app hidden", listing)
	}

	// Only chat completions are bridged; the legacy and Anthropic routes
	// still reject web models.
	w = httptest.NewRecorder()
	RootHandler(w, chatRequest("/v1/completions", map[string]any{"model": "img", "prompt": "x"}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("/v1/completions status = %d, want 400", w.Code)
	}

	// Without a user prompt there is nothing to draw.
	w = httptest.NewRecorder()
	RootHandler(w, chatRequest("/v1/chat/completions", map[string]any{
		"model": "img", "messages": []map[string]any{{"role": "system", "content": "x"}},
	}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("no-user-message status = %d, want 400", w.Code)
	}
	assertOpenAICode(t, w, "prompt_required")
}

func TestImageChat_StreamKeepsLinesSmall(t *testing.T) {
	b := newImageBackend(t)
	big := "iVBORw0KGgo" + strings.Repeat("QUJD", 256<<10) // ~1 MB of base64
	b.image = big
	setImageChatConfig(b)
	defer manager.ShutdownCurrentModel()

	w := httptest.NewRecorder()
	RootHandler(w, chatRequest("/v1/chat/completions", map[string]any{
		"model": "img", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "a huge canvas"}},
	}))

	var content strings.Builder
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if len(line) > 64<<10 {
			t.Fatalf("SSE line of %d bytes, want each line under 64 KB", len(line))
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk: %v", err)
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
	}
	want := "![Generated image](data:image/png;base64," + big + ")"
	if content.String() != want {
		t.Errorf("reassembled content differs (got %d bytes, want %d)", content.Len(), len(want))
	}
}
