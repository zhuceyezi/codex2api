package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/liveupdates"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type liveDataTicket struct {
	expires    time.Time
	credential string
	keyID      int64
}
type liveDataState struct {
	sync.Mutex
	tickets         map[string]liveDataTicket
	connections     int
	accountRevision uint64
}

func (h *Handler) issueLiveDataTicket(c *gin.Context, credential string, keyID int64) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeInternalError(c, err)
		return
	}
	ticket := hex.EncodeToString(raw[:])
	h.liveData.Lock()
	defer h.liveData.Unlock()
	if h.liveData.tickets == nil {
		h.liveData.tickets = make(map[string]liveDataTicket)
	}
	now := time.Now()
	for key, value := range h.liveData.tickets {
		if !now.Before(value.expires) {
			delete(h.liveData.tickets, key)
		}
	}
	if len(h.liveData.tickets) >= 256 || h.liveData.connections >= 128 {
		writeError(c, http.StatusTooManyRequests, "实时连接数量已达上限")
		return
	}
	h.liveData.tickets[ticket] = liveDataTicket{expires: now.Add(30 * time.Second), credential: credential, keyID: keyID}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"ticket": ticket})
}
func (h *Handler) GetAdminLiveDataTicket(c *gin.Context) {
	credential, _ := h.resolveAdminSecret(c.Request.Context())
	h.issueLiveDataTicket(c, credential, 0)
}
func (h *Handler) GetPublicLiveDataTicket(c *gin.Context) {
	row := h.publicAPIKeyUsageRow(c, c.Request.Context())
	if row == nil {
		return
	}
	h.issueLiveDataTicket(c, extractPublicAPIKey(c), row.ID)
}
func (h *Handler) liveDataAuthorized(ctx context.Context, ticket liveDataTicket) bool {
	if ticket.keyID == 0 {
		current, _ := h.resolveAdminSecret(ctx)
		return current != "" && security.SecureCompare(current, ticket.credential)
	}
	enabled, err := h.PublicAPIKeyUsagePageEnabled(ctx)
	if err != nil || !enabled {
		return false
	}
	row, err := h.db.GetAPIKeyByValue(ctx, ticket.credential)
	return err == nil && row.ID == ticket.keyID && row.Enabled
}

// Credentials stay in the authenticated ticket request. The upgrade URL only
// contains a single-use, 30-second random token. Gorilla enforces same Origin.
func (h *Handler) LiveDataSocket(c *gin.Context) {
	h.liveData.Lock()
	ticket, ok := h.liveData.tickets[c.Query("ticket")]
	delete(h.liveData.tickets, c.Query("ticket"))
	if !ok || !time.Now().Before(ticket.expires) || h.liveData.connections >= 128 {
		h.liveData.Unlock()
		writeError(c, http.StatusUnauthorized, "实时连接凭证无效或已过期")
		return
	}
	h.liveData.connections++
	h.liveData.Unlock()
	defer func() { h.liveData.Lock(); h.liveData.connections--; h.liveData.Unlock() }()
	if !h.liveDataAuthorized(c.Request.Context(), ticket) {
		writeError(c, http.StatusUnauthorized, "实时连接认证失败")
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1024)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	sub, unsubscribe := liveupdates.Subscribe(ticket.keyID)
	defer unsubscribe()
	send := func(kind string, topics liveupdates.Topics, immediate bool) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(gin.H{"type": kind, "topics": topics.Names(), "immediate": immediate, "at": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	h.syncLiveAccountCaches()
	if err := send("ready", liveupdates.All, false); err != nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-done:
			return
		case <-c.Request.Context().Done():
			return
		case <-sub.Immediate():
			topics, immediate := sub.TakeUpdate()
			if topics&liveupdates.Accounts != 0 {
				h.syncLiveAccountCaches()
			}
			if topics != 0 {
				if err := send("invalidate", topics, immediate); err != nil {
					return
				}
			}
		case <-ticker.C:
			ticks++
			topics, immediate := sub.TakeUpdate()
			if topics&liveupdates.Accounts != 0 {
				h.syncLiveAccountCaches()
			}
			// Runtime counters and relative time windows change without DB writes.
			if ticks%5 == 0 {
				topics |= liveupdates.Runtime
			}
			if ticks%60 == 0 {
				topics |= liveupdates.Usage
			}
			if ticks%15 == 0 {
				ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
				authorized := h.liveDataAuthorized(ctx, ticket)
				cancel()
				if !authorized {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, "authentication expired"), time.Now().Add(time.Second))
					return
				}
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
			if topics != 0 {
				if err := send("invalidate", topics, immediate); err != nil {
					return
				}
			}
		}
	}
}

func publishAdminLiveDataChange(c *gin.Context) {
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions || c.Writer.Status() >= 400 || strings.HasSuffix(c.Request.URL.Path, "/live-ticket") {
		return
	}
	topic := liveupdates.Accounts
	path := c.Request.URL.Path
	switch {
	case strings.Contains(path, "/usage"):
		topic = liveupdates.Usage
	case strings.HasPrefix(path, "/api/admin/keys"):
		topic = liveupdates.Keys
	case strings.Contains(path, "/proxies"):
		topic = liveupdates.Proxies
	case strings.Contains(path, "/settings"):
		topic = liveupdates.Settings
	}
	liveupdates.Publish(topic, 0)
}

func (h *Handler) syncLiveAccountCaches() {
	revision := liveupdates.AccountRevision()
	h.liveData.Lock()
	defer h.liveData.Unlock()
	if h.liveData.accountRevision != revision {
		h.invalidateAccountSnapshotCaches()
		h.liveData.accountRevision = revision
	}
}
