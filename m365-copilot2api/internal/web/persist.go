package web

import (
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// persistStore 延迟磁盘持久化：内存变更只标记 dirty，由后台循环合并写盘，
// 避免高频路径在锁内做整文件写入。
type persistStore struct {
	writeMu sync.Mutex
	dirtyMu sync.Mutex
	dirty   bool
	// registered 保证一个 store 只进 persistList 一次。见 ensurePersistLoop。
	registered atomic.Bool
	flush      func() error // 自行管理数据快照锁，锁外写盘
}

func (p *persistStore) markDirty() {
	p.dirtyMu.Lock()
	p.dirty = true
	p.dirtyMu.Unlock()
	ensurePersistLoop(p)
}

func (p *persistStore) flushPending() {
	p.dirtyMu.Lock()
	if !p.dirty {
		p.dirtyMu.Unlock()
		return
	}
	p.dirty = false
	p.dirtyMu.Unlock()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.flush(); err != nil {
		p.dirtyMu.Lock()
		p.dirty = true
		p.dirtyMu.Unlock()
		log.Printf("[persist] flush failed: %v", err)
	}
}

func (p *persistStore) flushNowBlocking() error {
	p.dirtyMu.Lock()
	p.dirty = false
	p.dirtyMu.Unlock()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.flush(); err != nil {
		p.dirtyMu.Lock()
		p.dirty = true
		p.dirtyMu.Unlock()
		return err
	}
	return nil
}

var (
	persistMu      sync.Mutex
	persistList    []*persistStore
	persistOnce    sync.Once
	persistStop    chan struct{}
	persistStopped chan struct{}
)

// ensurePersistLoop 注册一个 store 并保证后台循环已启动。
//
// 注册必须幂等：每次 markDirty 都无条件 append 时，persistList 会随写入次数
// 线性增长且永不回收 —— 一个长跑的网关每次会话/对话变更都往里塞一份同一个
// 指针，内存只增不减，且每轮 FlushAllPersist 都要复制并遍历这条越来越长的
// 列表（其中除一份之外全是重复项，进 flushPending 后因 dirty 已被清掉而空转）。
// 表现就是常驻内存和每 5 秒的 CPU 开销随运行时长一起爬升。
// 现在每个 store 只入列一次，列表长度等于 store 的个数。
func ensurePersistLoop(p *persistStore) {
	if p.registered.CompareAndSwap(false, true) {
		persistMu.Lock()
		persistList = append(persistList, p)
		persistMu.Unlock()
	}
	persistOnce.Do(func() {
		persistStop = make(chan struct{})
		persistStopped = make(chan struct{})
		go persistLoop()
	})
}

func persistLoop() {
	defer close(persistStopped)
	interval := 5 * time.Second
	if v := os.Getenv("M365_PERSIST_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 100*time.Millisecond {
			interval = d
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			FlushAllPersist()
		case <-persistStop:
			FlushAllPersist()
			return
		}
	}
}

// FlushAllPersist 同步落盘全部已注册的 store，供优雅停机调用。
func FlushAllPersist() {
	persistMu.Lock()
	list := append([]*persistStore(nil), persistList...)
	persistMu.Unlock()
	for _, p := range list {
		p.flushPending()
	}
}

var persistStopOnce sync.Once

// StopPersistLoop 停止后台循环并等待其完成最后一轮 flush。
func StopPersistLoop() {
	persistOnce.Do(func() {
		persistStop = make(chan struct{})
		persistStopped = make(chan struct{})
		go persistLoop()
	})
	persistStopOnce.Do(func() {
		close(persistStop)
	})
	<-persistStopped
}
