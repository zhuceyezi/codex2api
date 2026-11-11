package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Bound per-request memory without buffering the network stream. Large payloads
// remain usable and are explicitly marked as truncated in the admin viewer.
const conversationMaxBodyBytes = 8 << 20

type conversationCaptureKey struct{}
type conversationCapture struct {
	mu       sync.Mutex
	value    database.Conversation
	request  bytes.Buffer
	response bytes.Buffer
}

func (capture *conversationCapture) append(target *bytes.Buffer, p []byte) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	remaining := conversationMaxBodyBytes - target.Len()
	if len(p) > remaining {
		p = p[:remaining]
		capture.value.Truncated = true
	}
	_, _ = target.Write(p)
}

func captureConversationFrame(ctx context.Context, payload []byte) {
	capture, _ := ctx.Value(conversationCaptureKey{}).(*conversationCapture)
	if capture != nil {
		capture.append(&capture.response, payload)
		capture.append(&capture.response, []byte("\n"))
	}
}

type ConversationRecorder struct {
	db    *database.DB
	queue chan *database.Conversation
	stop  chan struct{}
	done  chan struct{}
}

func NewConversationRecorder(db *database.DB) (*ConversationRecorder, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.EnsureConversationSchema(ctx); err != nil {
		return nil, err
	}
	recorder := &ConversationRecorder{db: db, queue: make(chan *database.Conversation, 16), stop: make(chan struct{}), done: make(chan struct{})}
	go recorder.run()
	return recorder, nil
}

func (recorder *ConversationRecorder) persist(value *database.Conversation) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := recorder.db.StoreConversation(ctx, value); err != nil {
		log.Printf("[Conversation] 保存失败 request_id=%s error=%v", value.RequestID, err)
	}
}

func (recorder *ConversationRecorder) purge() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := recorder.db.PurgeConversations(ctx); err != nil {
		log.Printf("[Conversation] 清理失败: %v", err)
	}
}

func (recorder *ConversationRecorder) run() {
	defer close(recorder.done)
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	recorder.purge()
	for {
		select {
		case value := <-recorder.queue:
			recorder.persist(value)
		case <-ticker.C:
			recorder.purge()
		case <-recorder.stop:
			for {
				select {
				case value := <-recorder.queue:
					recorder.persist(value)
				default:
					return
				}
			}
		}
	}
}

func (recorder *ConversationRecorder) Close() { close(recorder.stop); <-recorder.done }

func (h *Handler) beginConversation(c *gin.Context, body []byte, contentType string) func() {
	if h.conversations == nil {
		return func() {}
	}
	capture := &conversationCapture{value: database.Conversation{Endpoint: c.Request.URL.Path, ContentType: contentType, CreatedAt: time.Now().Unix()}}
	capture.append(&capture.request, body)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), conversationCaptureKey{}, capture))
	return func() {
		capture.mu.Lock()
		value := capture.value
		value.RequestBody = capture.request.String()
		value.ResponseBody = capture.response.String()
		capture.mu.Unlock()
		value.RequestID = snapshotUpstreamTrace(c.Request.Context()).RequestID
		if value.RequestID == "" {
			return
		}
		// Queue after forwarding completes. Saturation falls back to an inline write
		// after the last response byte, rather than dropping a conversation.
		select {
		case h.conversations.queue <- &value:
		default:
			h.conversations.persist(&value)
		}
	}
}

type conversationHTTPWriter struct {
	gin.ResponseWriter
	capture *conversationCapture
}

func (writer *conversationHTTPWriter) Write(p []byte) (int, error) {
	n, err := writer.ResponseWriter.Write(p)
	if n > 0 {
		writer.capture.append(&writer.capture.response, p[:n])
	}
	return n, err
}
func (writer *conversationHTTPWriter) WriteString(s string) (int, error) {
	return writer.Write([]byte(s))
}

type conversationBodyReader struct {
	io.ReadCloser
	capture *conversationCapture
}

func (reader *conversationBodyReader) Read(p []byte) (int, error) {
	n, err := reader.ReadCloser.Read(p)
	if n > 0 {
		reader.capture.append(&reader.capture.request, p[:n])
	}
	return n, err
}

func (h *Handler) conversationMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimPrefix(c.Request.URL.Path, "/v1")
		path = strings.TrimPrefix(path, "/backend-api/codex")
		supported := map[string]bool{"/chat/completions": true, "/responses": true, "/responses/compact": true, "/messages": true, "/images/generations": true, "/images/edits": true, "/videos/generations": true, "/videos/edits": true, "/videos/extensions": true}
		if h.conversations == nil || c.Request.Method != http.MethodPost || !supported[path] || strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
			c.Next()
			return
		}
		finish := h.beginConversation(c, nil, "")
		capture := c.Request.Context().Value(conversationCaptureKey{}).(*conversationCapture)
		// Multipart buffers may already contain binary attachments. Retain only a
		// marker instead of copying the cached raw bytes into the database.
		multipart := strings.Contains(c.GetHeader("Content-Type"), "multipart/")
		if multipart {
			capture.append(&capture.request, []byte("[multipart request: binary file content is not retained]"))
		} else if body, ok := c.Get("raw_body"); ok {
			if b, ok := body.([]byte); ok {
				capture.append(&capture.request, b)
			}
		}
		if !multipart && capture.request.Len() == 0 && c.Request.Body != nil {
			c.Request.Body = &conversationBodyReader{ReadCloser: c.Request.Body, capture: capture}
		}
		original := c.Writer
		c.Writer = &conversationHTTPWriter{ResponseWriter: original, capture: capture}
		defer func() {
			capture.mu.Lock()
			capture.value.ContentType = c.Writer.Header().Get("Content-Type")
			capture.mu.Unlock()
			c.Writer = original
			finish()
		}()
		c.Next()
	}
}

func (h *Handler) SetConversationRecorder(recorder *ConversationRecorder) { h.conversations = recorder }
