package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *Handler) GetUsageConversation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效请求记录")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	value, err := h.db.ConversationForUsageLog(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "此记录没有留存正文，或已超过保留期限")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *Handler) GetConversationRetention(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	days, err := h.db.ConversationRetentionDays(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"retention_days": days, "max_body_bytes": 8 << 20})
}

func (h *Handler) UpdateConversationRetention(c *gin.Context) {
	var req struct {
		RetentionDays int `json:"retention_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.RetentionDays < 1 || req.RetentionDays > 365 {
		writeError(c, http.StatusBadRequest, "保留天数必须在 1 到 365 之间")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.db.SetConversationRetentionDays(ctx, req.RetentionDays); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"retention_days": req.RetentionDays})
}
