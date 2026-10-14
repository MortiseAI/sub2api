package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

type codexQuotaReader interface {
	QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error)
}

// BindKeyQuotaReader is called by dependency injection before serving requests.
func (s *OpenAIGatewayService) BindKeyQuotaReader(reader codexQuotaReader) {
	s.keyQuotaReader = reader
}

type KeyCodexQuotaWindow struct {
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	WindowMinutes    int64   `json:"window_minutes"`
	ResetAt          int64   `json:"reset_at,omitempty"`
}

// Deliberately omit upstream account IDs, email and credentials.
type KeyCodexQuota struct {
	Object        string                          `json:"object"`
	SchemaVersion int                             `json:"schema_version"`
	Selection     string                          `json:"selection"`
	AccountRef    string                          `json:"account_ref"`
	ObservedAtMs  int64                           `json:"observed_at_ms"`
	PlanType      string                          `json:"plan_type,omitempty"`
	RateLimits    map[string]*KeyCodexQuotaWindow `json:"rate_limits"`
}

type cachedKeyCodexQuota struct {
	usage     *OpenAIQuotaUsage
	expiresAt time.Time
}

// FetchKeyCodexQuota reads the single eligible account, or this key/model's
// latest actual account. It never reserves a scheduler slot or consumes credits.
func (s *OpenAIGatewayService) FetchKeyCodexQuota(ctx context.Context, key *APIKey, model string) (*KeyCodexQuota, error) {
	if key == nil || key.GroupID == nil || key.Group == nil || key.Group.ID != *key.GroupID || key.Group.Platform != PlatformOpenAI {
		return nil, infraerrors.New(http.StatusBadRequest, "CODEX_QUOTA_UNSUPPORTED", "Codex quota requires an OpenAI group")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "CODEX_QUOTA_MODEL_REQUIRED", "model is required")
	}
	if key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(model) {
		return nil, infraerrors.New(http.StatusForbidden, "CODEX_QUOTA_MODEL_DENIED", "Model is not allowed by this group")
	}
	if s == nil || s.accountRepo == nil || s.keyQuotaReader == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "CODEX_QUOTA_UNAVAILABLE", "Codex quota is unavailable")
	}
	accounts, err := s.accountRepo.ListByGroup(ctx, *key.GroupID)
	if err != nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "CODEX_QUOTA_UNAVAILABLE", "Failed to load quota accounts")
	}
	eligible := make(map[int64]*Account)
	for i := range accounts {
		// Model lookup memoizes on Account; keep that mutation request-local.
		accountCopy := accounts[i]
		account := &accountCopy
		// Include exhausted windows so users can see 0%, but exclude disabled,
		// expired and unrelated model accounts. Do not call IsSchedulable().
		if account.IsOpenAIOAuth() && isPinnedCodexModelsAccountUsable(account) && account.IsModelSupported(model) {
			eligible[account.ID] = account
		}
	}
	var accountID int64
	selection := "single_account"
	if len(eligible) == 1 {
		for id := range eligible {
			accountID = id
		}
	} else if len(eligible) > 1 && s.usageLogRepo != nil {
		logs, _, lookupErr := s.usageLogRepo.ListWithFilters(ctx,
			pagination.PaginationParams{Page: 1, PageSize: 1, SortBy: "id", SortOrder: pagination.SortOrderDesc},
			usagestats.UsageLogFilters{UserID: key.UserID, APIKeyID: key.ID, GroupID: *key.GroupID, Model: model, ModelFilterSource: "requested"})
		if lookupErr != nil {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "CODEX_QUOTA_UNAVAILABLE", "Failed to identify the quota account")
		}
		if len(logs) > 0 && eligible[logs[0].AccountID] != nil {
			accountID = logs[0].AccountID
			selection = "last_used_account"
		}
	}
	if accountID == 0 {
		return nil, infraerrors.New(http.StatusConflict, "CODEX_QUOTA_ACCOUNT_UNKNOWN", "No unique quota account; send a model request first")
	}
	value, err, _ := s.keyQuotaFlight.Do(strconv.FormatInt(accountID, 10), func() (any, error) {
		now := time.Now()
		if cached, ok := s.keyQuotaCache.Load(accountID); ok {
			entry := cached.(cachedKeyCodexQuota)
			if entry.expiresAt.After(now) && !keyCodexQuotaWindowExpired(entry.usage, now) {
				return entry.usage, nil
			}
		}
		queryCtx, cancel := context.WithTimeout(ctx, 22*time.Second)
		defer cancel()
		usage, queryErr := s.keyQuotaReader.QueryUsage(queryCtx, accountID)
		if queryErr != nil || usage == nil {
			// Upstream errors can contain identity data; keep the public error narrow.
			return nil, infraerrors.New(http.StatusBadGateway, "CODEX_QUOTA_QUERY_FAILED", "Failed to query upstream Codex quota")
		}
		if usage.FetchedAt <= 0 {
			usage.FetchedAt = time.Now().Unix()
		}
		s.keyQuotaCache.Store(accountID, cachedKeyCodexQuota{usage: usage, expiresAt: time.Now().Add(time.Minute)})
		return usage, nil
	})
	if err != nil {
		return nil, err
	}
	usage := value.(*OpenAIQuotaUsage)
	limits := make(map[string]*KeyCodexQuotaWindow)
	rateLimit := usage.RateLimit
	if eligible[accountID].IsShadow() || isCodexSparkModel(eligible[accountID].GetMappedModel(model)) {
		rateLimit = nil
		for _, extra := range usage.AdditionalRateLimits {
			if extra.MeteredFeature == "codex_bengalfox" {
				rateLimit = extra.RateLimit
				break
			}
		}
	}
	if rateLimit != nil {
		for _, raw := range []*OpenAIRateLimitWindow{rateLimit.PrimaryWindow, rateLimit.SecondaryWindow} {
			if raw == nil || math.IsNaN(raw.UsedPercent) || math.IsInf(raw.UsedPercent, 0) {
				continue
			}
			name := ""
			switch raw.LimitWindowSeconds {
			case 5 * 60 * 60:
				name = "primary"
			case 7 * 24 * 60 * 60:
				name = "weekly"
			default:
				continue
			}
			used := math.Max(0, math.Min(100, raw.UsedPercent))
			reset := raw.ResetAt
			if reset <= 0 && raw.ResetAfterSeconds > 0 {
				reset = usage.FetchedAt + raw.ResetAfterSeconds
			}
			limits[name] = &KeyCodexQuotaWindow{UsedPercent: used, RemainingPercent: 100 - used, WindowMinutes: raw.LimitWindowSeconds / 60, ResetAt: reset}
		}
	}
	if len(limits) == 0 {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEX_QUOTA_NO_DATA", "Upstream Codex quota windows are unavailable")
	}
	mac := hmac.New(sha256.New, []byte(key.Key))
	_, _ = fmt.Fprintf(mac, "%d:%d", key.ID, accountID)
	return &KeyCodexQuota{Object: "sub2api.codex_quota", SchemaVersion: 1, Selection: selection,
		AccountRef: hex.EncodeToString(mac.Sum(nil))[:24], ObservedAtMs: usage.FetchedAt * 1000,
		PlanType: usage.PlanType, RateLimits: limits}, nil
}

func keyCodexQuotaWindowExpired(usage *OpenAIQuotaUsage, now time.Time) bool {
	if usage == nil || usage.RateLimit == nil {
		return true
	}
	windows := []*OpenAIRateLimitWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow}
	for _, extra := range usage.AdditionalRateLimits {
		if extra.RateLimit != nil {
			windows = append(windows, extra.RateLimit.PrimaryWindow, extra.RateLimit.SecondaryWindow)
		}
	}
	for _, window := range windows {
		if window == nil {
			continue
		}
		reset := window.ResetAt
		if reset <= 0 && window.ResetAfterSeconds > 0 {
			reset = usage.FetchedAt + window.ResetAfterSeconds
		}
		if reset > 0 && reset <= now.Unix() {
			return true
		}
	}
	return false
}
