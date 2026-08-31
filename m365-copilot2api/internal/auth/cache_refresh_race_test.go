package auth

import (
	"errors"
	"testing"
	"time"
)

// 刷新失败时只能改 Status，不能把整份旧快照写回去。
//
// refreshInflight 在锁外调用 AAD 刷新，手里拿的是调用前的 AccountToken 快照。
// 修复前失败分支把整个快照写回存储：一个并发的 Upsert（例如同一时间 ROPC 重新
// 授权成功，或另一个账号路径刚存进新令牌）刚写好的 access/refresh token 和过期
// 时间，会被这份旧快照原样覆盖掉。输的那一方不是「没抢到」，而是把赢的那一方
// 已经落盘的结果抹掉了 —— 一次失败的刷新反而让一个刚刚变好的账号退回旧令牌。
//
// 这里用注入的刷新函数在「快照已取、写回未发生」的窗口里做那次并发 Upsert，
// 把竞态变成确定性的顺序。
func TestRefreshFailureKeepsConcurrentUpdate(t *testing.T) {
	storePath := isolateStoreKey(t)
	store, err := OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{
		HomeOID:      "oid-1",
		Email:        "a@example.com",
		DisplayName:  "A",
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		TenantID:     "tid-1",
		ExpiresAt:    time.Now().Add(-time.Hour), // 已过期，EnsureValid 必然去刷新
	}); err != nil {
		t.Fatal(err)
	}

	oldRefresh := refreshFunc
	t.Cleanup(func() { refreshFunc = oldRefresh })
	refreshFunc = func(string) (TokenSet, error) {
		// 刷新在飞行中：另一条路径存进了一份全新的令牌。
		if _, err := store.Upsert(TokenSet{
			HomeOID:      "oid-1",
			Email:        "a@example.com",
			DisplayName:  "A",
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			TenantID:     "tid-1",
			ExpiresAt:    time.Now().Add(time.Hour),
		}); err != nil {
			t.Errorf("并发 Upsert 失败: %v", err)
		}
		return TokenSet{}, errors.New("invalid_grant: refresh token已被吊销")
	}

	acc, err := store.EnsureValid("oid-1")
	if err == nil {
		t.Fatal("刷新失败却返回成功")
	}
	if acc.Status != "expired" {
		t.Errorf("返回的 Status=%q want expired", acc.Status)
	}

	// 核心断言：并发写入的新令牌必须存活。
	stored, ok := store.Get("oid-1")
	if !ok {
		t.Fatal("账号丢失")
	}
	if stored.AccessToken != "fresh-access" {
		t.Errorf("存储里的 AccessToken=%q want fresh-access："+
			"失败的刷新把并发写入的新令牌覆盖成了旧快照", stored.AccessToken)
	}
	if stored.RefreshToken != "fresh-refresh" {
		t.Errorf("存储里的 RefreshToken=%q want fresh-refresh："+
			"refresh token 被旧快照覆盖后，这个账号再也刷不回来", stored.RefreshToken)
	}
	if !stored.ExpiresAt.After(time.Now()) {
		t.Errorf("存储里的 ExpiresAt=%v 已过期，说明旧快照的过期时间被写回了", stored.ExpiresAt)
	}
	if stored.Status != "expired" {
		t.Errorf("存储里的 Status=%q want expired", stored.Status)
	}

	// 返回给调用方的也必须是当前值，而不是过时的快照。
	if acc.AccessToken != "fresh-access" {
		t.Errorf("返回的 AccessToken=%q want fresh-access（返回了过时快照）", acc.AccessToken)
	}

	// 落盘内容同样不能是旧快照。
	reopened, err := OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reopened.Get("oid-1")
	if !ok {
		t.Fatal("重新打开后账号丢失")
	}
	if persisted.RefreshToken != "fresh-refresh" {
		t.Errorf("落盘的 RefreshToken=%q want fresh-refresh", persisted.RefreshToken)
	}
}

// markStatus 只改 Status，其余字段一个都不能动。
func TestMarkStatusTouchesOnlyStatus(t *testing.T) {
	storePath := isolateStoreKey(t)
	store, err := OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Upsert(TokenSet{
		HomeOID:      "oid-1",
		Email:        "a@example.com",
		DisplayName:  "A",
		AccessToken:  "access",
		RefreshToken: "refresh",
		TenantID:     "tid-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	after, ok := store.markStatus("oid-1", "expired")
	if !ok {
		t.Fatal("markStatus 找不到账号")
	}
	if after.Status != "expired" {
		t.Errorf("Status=%q want expired", after.Status)
	}
	// 把 Status 对齐后，两份结构必须完全相等。
	before.Status = "expired"
	if after != before {
		t.Errorf("markStatus 改动了 Status 之外的字段：\nbefore=%+v\nafter =%+v", before, after)
	}

	// 不存在的账号必须报告未找到，而不是凭空插入一条。
	if _, ok := store.markStatus("no-such-account", "expired"); ok {
		t.Error("markStatus 对不存在的账号报告了成功")
	}
	if len(store.List()) != 1 {
		t.Errorf("账号数量=%d want 1", len(store.List()))
	}
}

// 没有 refresh token 时同样只改 Status，并返回当前值。
func TestEnsureValidWithoutRefreshTokenOnlyChangesStatus(t *testing.T) {
	storePath := isolateStoreKey(t)
	store, err := OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{
		HomeOID:     "oid-1",
		Email:       "a@example.com",
		DisplayName: "A",
		AccessToken: "access",
		TenantID:    "tid-1",
		ExpiresAt:   time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	acc, err := store.EnsureValid("oid-1")
	if err == nil {
		t.Fatal("缺少 refresh token 却返回成功")
	}
	if acc.Status != "expired" {
		t.Errorf("Status=%q want expired", acc.Status)
	}
	if acc.AccessToken != "access" || acc.Email != "a@example.com" || acc.TID != "tid-1" {
		t.Errorf("其余字段被改动: %+v", acc)
	}
	stored, _ := store.Get("oid-1")
	if stored.Status != "expired" || stored.AccessToken != "access" {
		t.Errorf("存储内容=%+v", stored)
	}
}
