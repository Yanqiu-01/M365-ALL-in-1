package web

import (
	"testing"
	"time"
)

// TPM 必须是尾随窗口内的真实速率：窗口外的记录一律不计。
func TestTokenRateWithinTrailingWindow(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := &usageLog{records: []UsageRecord{
		// 窗口外（2 分钟前），必须被排除
		{Time: now.Add(-2 * time.Minute), InputTokens: 1000, OutputTokens: 1000},
		// 窗口内
		{Time: now.Add(-30 * time.Second), InputTokens: 100, OutputTokens: 50, CacheTokens: 10},
		{Time: now.Add(-5 * time.Second), InputTokens: 200, OutputTokens: 40},
	}}
	got := log.tokenRateWithin(now, time.Minute)
	// 160 + 240 = 400
	if got.Tokens != 400 {
		t.Errorf("Tokens = %d, want 400 (out-of-window record must be excluded)", got.Tokens)
	}
	if got.Requests != 2 {
		t.Errorf("Requests = %d, want 2", got.Requests)
	}
	// 窗口本身是 1 分钟，TPM 等于窗口内 token 数
	if got.TPM != 400 {
		t.Errorf("TPM = %d, want 400", got.TPM)
	}
	if got.RPM != 2 {
		t.Errorf("RPM = %d, want 2", got.RPM)
	}
	if got.WindowSeconds != 60 {
		t.Errorf("WindowSeconds = %d, want 60", got.WindowSeconds)
	}
}

// 非 1 分钟窗口要归一化到每分钟。
func TestTokenRateScalesToPerMinute(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := &usageLog{records: []UsageRecord{
		{Time: now.Add(-10 * time.Second), InputTokens: 100},
	}}
	// 30 秒窗口内 100 token → 每分钟 200
	got := log.tokenRateWithin(now, 30*time.Second)
	if got.TPM != 200 {
		t.Errorf("TPM = %d, want 200 (100 tokens per 30s scales to 200/min)", got.TPM)
	}
}

// 空日志不得 panic，速率为 0。
func TestTokenRateEmptyLog(t *testing.T) {
	got := (&usageLog{}).tokenRateWithin(time.Now(), time.Minute)
	if got.Tokens != 0 || got.TPM != 0 || got.Requests != 0 {
		t.Errorf("empty log rate = %+v, want zeros", got)
	}
}

// token 命中率 = 命中 token / 总 token，分母含未发出的部分。
func TestTokenHitRateDefinition(t *testing.T) {
	s := &CacheStats{KeyStats: map[string]*KeyStat{}}
	s.persist = &persistStore{flush: func() error { return nil }}
	// 一次命中：发出 100，省下 300 → 300/400 = 75%
	s.RecordRequest("k", true, 100, 300, 1)
	if got := s.TokenHitRate; got < 74.9 || got > 75.1 {
		t.Errorf("TokenHitRate = %v, want 75", got)
	}
	// 按请求数的命中率仍保留：1/1 = 100%
	if got := s.HitRate; got < 99.9 {
		t.Errorf("HitRate = %v, want 100 (request-count ratio preserved)", got)
	}
	// 每个 key 的口径一致
	if got := s.KeyStats["k"].TokenHitRate; got < 74.9 || got > 75.1 {
		t.Errorf("per-key TokenHitRate = %v, want 75", got)
	}
}

// 零 token 时不得除零。
func TestTokenHitRateZeroTokens(t *testing.T) {
	s := &CacheStats{KeyStats: map[string]*KeyStat{}}
	s.persist = &persistStore{flush: func() error { return nil }}
	s.RecordRequest("k", false, 0, 0, 0)
	if s.TokenHitRate != 0 {
		t.Errorf("TokenHitRate = %v, want 0 for zero tokens", s.TokenHitRate)
	}
}
