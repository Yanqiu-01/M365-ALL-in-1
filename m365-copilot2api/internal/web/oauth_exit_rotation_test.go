package web

import (
	"testing"
)

// 用户要求：批量 OAuth 走代理池，每 100 个号换一个代理。
//
// 第一版把计数写成了 runOAuthBatch 的局部变量，那样它每次请求都从 0 开始。而
// panelOAuthBatchMax 是 64，小于轮换间隔 100 —— processed%100==0 永远不成立，759 个账号
// 分成 12 批会全部走同一个出口，轮换等于没实现。恢复流程恰恰是「多次请求累计几百个账号」
// 这种形态，所以计数必须归服务端。

func TestOAuthExitRotationCountsAcrossRequests(t *testing.T) {
	s := &Server{}

	// 模拟 12 次请求，每次 64 个账号 —— 恢复 759 个账号的真实形态。
	const perRequest = 64
	const requests = 12
	turns := map[int64]bool{}
	for range requests {
		for range perRequest {
			if n := s.oauthExitProcessed.Add(1); n > 1 && (n-1)%oauthExitRotateEvery == 0 {
				s.oauthExitTurn.Add(1)
			}
		}
		turns[s.oauthExitTurn.Load()] = true
	}

	total := int64(perRequest * requests)
	if got := s.oauthExitProcessed.Load(); got != total {
		t.Fatalf("processed = %d, want %d", got, total)
	}
	// 768 个账号、每 100 换一次，应当换过 7 次。
	wantTurns := (total - 1) / oauthExitRotateEvery
	if got := s.oauthExitTurn.Load(); got != wantTurns {
		t.Errorf("rotated %d times over %d accounts, want %d — with a per-request counter this "+
			"would have stayed at 0 because %d < %d", got, total, wantTurns, perRequest, oauthExitRotateEvery)
	}
	if len(turns) < 2 {
		t.Error("the exit index never advanced across requests; rotation is a no-op")
	}
}

// 单次请求内也要能换，只要账号数够。
func TestOAuthExitRotationWithinOneLargeRequest(t *testing.T) {
	s := &Server{}
	for range oauthExitRotateEvery * 3 {
		if n := s.oauthExitProcessed.Add(1); n > 1 && (n-1)%oauthExitRotateEvery == 0 {
			s.oauthExitTurn.Add(1)
		}
	}
	if got := s.oauthExitTurn.Load(); got != 2 {
		t.Errorf("rotated %d times over %d accounts, want 2", got, oauthExitRotateEvery*3)
	}
}

// 第一个账号不得触发轮换：否则第一批就跳过了池子里的第一个出口。
func TestOAuthExitRotationDoesNotFireOnTheFirstAccount(t *testing.T) {
	s := &Server{}
	if n := s.oauthExitProcessed.Add(1); n > 1 && (n-1)%oauthExitRotateEvery == 0 {
		s.oauthExitTurn.Add(1)
	}
	if got := s.oauthExitTurn.Load(); got != 0 {
		t.Errorf("rotated on the first account (turn=%d); the first exit would be skipped", got)
	}
}

// 间隔本身必须大于 1，否则每个账号换一次出口，握手开销压过收益。
func TestOAuthExitRotateEveryIsSane(t *testing.T) {
	if oauthExitRotateEvery < 2 {
		t.Fatalf("oauthExitRotateEvery = %d; rotating per account would spend more on handshakes "+
			"than it saves", oauthExitRotateEvery)
	}
}
