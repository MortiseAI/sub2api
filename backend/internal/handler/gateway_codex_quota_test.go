package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type codexQuotaHandlerAccounts struct{ service.AccountRepository }

func (*codexQuotaHandlerAccounts) ListByGroup(_ context.Context, id int64) ([]service.Account, error) {
	return []service.Account{{ID: 101, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"client-model": "upstream-model"}}}}, nil
}

type codexQuotaHandlerReader struct{}

func (codexQuotaHandlerReader) QueryUsage(context.Context, int64) (*service.OpenAIQuotaUsage, error) {
	return &service.OpenAIQuotaUsage{FetchedAt: time.Now().Unix(), Email: "private@example.com", RateLimit: &service.OpenAIRateLimit{
		PrimaryWindow:   &service.OpenAIRateLimitWindow{UsedPercent: 42, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
		SecondaryWindow: &service.OpenAIRateLimitWindow{UsedPercent: 17, LimitWindowSeconds: 604800, ResetAfterSeconds: 86400},
	}}, nil
}

func TestGatewayCodexQuotaHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gateway := service.NewOpenAIGatewayService(&codexQuotaHandlerAccounts{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gateway.BindKeyQuotaReader(codexQuotaHandlerReader{})
	handler := &GatewayHandler{openAIGatewayService: gateway}
	groupID := int64(7)
	key := &service.APIKey{ID: 11, UserID: 22, Key: "test-key", GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}}
	r := gin.New()
	r.GET("/v1/sub2api/quota", func(c *gin.Context) { c.Set(string(middleware2.ContextKeyAPIKey), key); handler.CodexQuota(c) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sub2api/quota?model=client-model", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var data map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
	require.Equal(t, "sub2api.codex_quota", data["object"])
	require.Equal(t, float64(1), data["schema_version"])
	limits := data["rate_limits"].(map[string]any)
	require.Equal(t, float64(58), limits["primary"].(map[string]any)["remaining_percent"])
	require.Equal(t, float64(83), limits["weekly"].(map[string]any)["remaining_percent"])
	require.NotContains(t, w.Body.String(), "private@example.com")
	require.NotContains(t, w.Body.String(), "test-key")
}
