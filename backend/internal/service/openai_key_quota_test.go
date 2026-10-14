package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type keyQuotaAccounts struct {
	AccountRepository
	accounts []Account
}

func (r *keyQuotaAccounts) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	if groupID != 7 {
		return nil, errors.New("wrong group")
	}
	return r.accounts, nil
}

type keyQuotaLogs struct {
	UsageLogRepository
	logs    []UsageLog
	filters usagestats.UsageLogFilters
	params  pagination.PaginationParams
}

func (r *keyQuotaLogs) ListWithFilters(_ context.Context, p pagination.PaginationParams, f usagestats.UsageLogFilters) ([]UsageLog, *pagination.PaginationResult, error) {
	r.filters, r.params = f, p
	return r.logs, nil, nil
}

type keyQuotaReaderStub struct {
	calls     atomic.Int32
	accountID atomic.Int64
	usage     *OpenAIQuotaUsage
	err       error
	entered   chan struct{}
	release   chan struct{}
}

func (r *keyQuotaReaderStub) QueryUsage(_ context.Context, accountID int64) (*OpenAIQuotaUsage, error) {
	r.calls.Add(1)
	r.accountID.Store(accountID)
	if r.entered != nil {
		r.entered <- struct{}{}
		<-r.release
	}
	return r.usage, r.err
}
func keyQuotaFixture() (*OpenAIGatewayService, *APIKey, *keyQuotaReaderStub, *keyQuotaAccounts) {
	groupID := int64(7)
	key := &APIKey{ID: 11, UserID: 22, Key: "secret-key", GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI}}
	repo := &keyQuotaAccounts{accounts: []Account{{ID: 101, Status: StatusActive, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"client-model": "upstream-model"}}}}}
	reader := &keyQuotaReaderStub{usage: &OpenAIQuotaUsage{Email: "private@example.com", AccountID: "private-account-id", FetchedAt: time.Now().Unix(), PlanType: "pro",
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 17, LimitWindowSeconds: 604800, ResetAfterSeconds: 86400},
			SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 42, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600}}}}
	return &OpenAIGatewayService{accountRepo: repo, keyQuotaReader: reader}, key, reader, repo
}

func TestKeyCodexQuotaNormalizesWindowsCachesAndRedacts(t *testing.T) {
	s, key, reader, _ := keyQuotaFixture()
	first, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Equal(t, "single_account", first.Selection)
	require.Equal(t, float64(58), first.RateLimits["primary"].RemainingPercent)
	require.Equal(t, float64(83), first.RateLimits["weekly"].RemainingPercent)
	require.Equal(t, reader.usage.FetchedAt+3600, first.RateLimits["primary"].ResetAt)
	second, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Equal(t, int32(1), reader.calls.Load())
	require.Equal(t, first.ObservedAtMs, second.ObservedAtMs)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	for _, secret := range []string{"private@example.com", "private-account-id", "secret-key"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Len(t, first.AccountRef, 24)
}

func TestKeyCodexQuotaDoesNotGuessAndScopesLatestAccountToKeyGroupAndModel(t *testing.T) {
	s, key, reader, repo := keyQuotaFixture()
	second := repo.accounts[0]
	second.ID = 102
	repo.accounts = append(repo.accounts, second)
	_, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.Error(t, err)
	require.Zero(t, reader.calls.Load())
	logs := &keyQuotaLogs{logs: []UsageLog{{AccountID: 102}}}
	s.usageLogRepo = logs
	result, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Equal(t, "last_used_account", result.Selection)
	require.Equal(t, int64(102), reader.accountID.Load())
	require.Equal(t, key.ID, logs.filters.APIKeyID)
	require.Equal(t, key.UserID, logs.filters.UserID)
	require.Equal(t, *key.GroupID, logs.filters.GroupID)
	require.Equal(t, "client-model", logs.filters.Model)
	require.Equal(t, "requested", logs.filters.ModelFilterSource)
	require.Equal(t, "desc", logs.params.SortOrder)
	logs.logs = []UsageLog{{AccountID: 999}}
	_, err = s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.Error(t, err)
	require.Equal(t, int32(1), reader.calls.Load(), "must not query accounts outside the eligible group")
}

func TestKeyCodexQuotaKeepsExhaustedAccountsButRejectsDisabledAndUnsupportedModels(t *testing.T) {
	s, key, reader, repo := keyQuotaFixture()
	reset := time.Now().Add(time.Hour)
	repo.accounts[0].RateLimitResetAt = &reset
	reader.usage.RateLimit.SecondaryWindow.UsedPercent = 100
	result, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Zero(t, result.RateLimits["primary"].RemainingPercent)
	_, err = s.FetchKeyCodexQuota(context.Background(), key, "other-model")
	require.Error(t, err)
	repo.accounts[0].Schedulable = false
	_, err = s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.Error(t, err)
}

func TestKeyCodexQuotaDoesNotFabricateMissingWindowsOrExposeUpstreamErrors(t *testing.T) {
	s, key, reader, _ := keyQuotaFixture()
	reader.err = errors.New("private@example.com secret-key")
	_, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "private@example.com"))
	reader.err = nil
	reader.usage.RateLimit = nil
	_, err = s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.Error(t, err)
}

func TestKeyCodexQuotaRequeriesExpiredCacheAndWindows(t *testing.T) {
	s, key, reader, _ := keyQuotaFixture()
	_, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	s.keyQuotaCache.Store(int64(101), cachedKeyCodexQuota{usage: reader.usage, expiresAt: time.Now().Add(-time.Second)})
	_, err = s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Equal(t, int32(2), reader.calls.Load())
	old := &OpenAIQuotaUsage{FetchedAt: time.Now().Unix(), RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{ResetAt: time.Now().Unix() - 1}}}
	s.keyQuotaCache.Store(int64(101), cachedKeyCodexQuota{usage: old, expiresAt: time.Now().Add(time.Minute)})
	_, err = s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Equal(t, int32(3), reader.calls.Load())
}

func TestKeyCodexQuotaCoalescesConcurrentUpstreamReads(t *testing.T) {
	s, key, reader, _ := keyQuotaFixture()
	reader.entered, reader.release = make(chan struct{}, 1), make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
			errs <- err
		}()
	}
	<-reader.entered
	close(reader.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), reader.calls.Load())
}

func TestKeyCodexQuotaSparkUsesItsFeatureWindows(t *testing.T) {
	s, key, reader, repo := keyQuotaFixture()
	repo.accounts[0].Credentials = map[string]any{"model_mapping": map[string]any{"client-model": "gpt-5.3-codex-spark"}}
	reader.usage.AdditionalRateLimits = []OpenAIAdditionalRateLimit{{MeteredFeature: "codex_bengalfox", RateLimit: &OpenAIRateLimit{
		PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 75, LimitWindowSeconds: 18000},
	}}}
	result, err := s.FetchKeyCodexQuota(context.Background(), key, "client-model")
	require.NoError(t, err)
	require.Equal(t, float64(25), result.RateLimits["primary"].RemainingPercent)
	require.NotContains(t, result.RateLimits, "weekly", "global Codex limits must not fill a missing Spark window")
}
