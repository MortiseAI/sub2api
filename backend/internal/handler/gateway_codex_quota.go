package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// GET /v1/sub2api/quota?model=... uses the model API key, never admin credentials.
func (h *GatewayHandler) CodexQuota(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	quota, err := h.openAIGatewayService.FetchKeyCodexQuota(c.Request.Context(), key, c.Query("model"))
	if err != nil {
		// Service errors are already sanitized for this public API.
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, quota)
}
