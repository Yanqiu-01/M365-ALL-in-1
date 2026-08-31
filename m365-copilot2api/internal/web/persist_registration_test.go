package web

import (
	"path/filepath"
	"testing"
)

// persistListLen 读取全局注册表长度。
func persistListLen() int {
	persistMu.Lock()
	defer persistMu.Unlock()
	return len(persistList)
}

// persistListCount 数某个 store 在注册表里出现了几次。
func persistListCount(target *persistStore) int {
	persistMu.Lock()
	defer persistMu.Unlock()
	n := 0
	for _, p := range persistList {
		if p == target {
			n++
		}
	}
	return n
}

// markDirty 以前每次调用都往 persistList 追加一份同一个指针，永不回收。
// 一个长跑的网关每写一次会话/对话就泄漏一个 slot，同时 FlushAllPersist 每 5 秒
// 要遍历这条只增不减的列表。这里用 1000 次 markDirty 断言注册是幂等的。
func TestMarkDirtyRegistersEachStoreOnce(t *testing.T) {
	before := persistListLen()
	store := &persistStore{flush: func() error { return nil }}

	const calls = 1000
	for i := 0; i < calls; i++ {
		store.markDirty()
	}

	if got := persistListCount(store); got != 1 {
		t.Fatalf("store appears %d times in persistList after %d markDirty calls, want 1", got, calls)
	}
	if grew := persistListLen() - before; grew != 1 {
		t.Fatalf("persistList grew by %d after %d markDirty calls, want 1", grew, calls)
	}
}

// 多个 store 各自注册一次，互不影响；注册幂等不能退化成「只收第一个」。
func TestMarkDirtyRegistersEveryDistinctStore(t *testing.T) {
	before := persistListLen()
	stores := make([]*persistStore, 3)
	for i := range stores {
		stores[i] = &persistStore{flush: func() error { return nil }}
		for j := 0; j < 5; j++ {
			stores[i].markDirty()
		}
	}
	if grew := persistListLen() - before; grew != len(stores) {
		t.Fatalf("persistList grew by %d, want %d", grew, len(stores))
	}
	for i, store := range stores {
		if got := persistListCount(store); got != 1 {
			t.Fatalf("store %d appears %d times, want 1", i, got)
		}
	}
}

// 幂等注册不得改变实际落盘行为：脏页仍然会被 FlushAllPersist 写出去。
func TestRegisteredStoreStillFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persisted.json")
	written := 0
	store := &persistStore{flush: func() error {
		written++
		return writeFileAtomic(path, []byte("{}"), 0o600)
	}}

	store.markDirty()
	store.markDirty()
	FlushAllPersist()
	if written == 0 {
		t.Fatal("dirty store was never flushed")
	}
	first := written

	// 已经落盘、没有新的 markDirty：不应再写一次。
	FlushAllPersist()
	if written != first {
		t.Fatalf("clean store flushed again: %d -> %d", first, written)
	}
}
