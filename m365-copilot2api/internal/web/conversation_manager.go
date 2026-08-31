package web

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

type ConversationCleanupMode string

const (
	CleanupAfterResponse ConversationCleanupMode = "after_response"
	CleanupOnExit        ConversationCleanupMode = "on_exit"
	CleanupKeepN         ConversationCleanupMode = "keep_n"
	CleanupMaxAge        ConversationCleanupMode = "max_age"
)

type managedConversation struct {
	ID         string    `json:"id"`
	AccountID  string    `json:"accountId"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	Title      string    `json:"title,omitempty"`
}

type conversationManager struct {
	mu        sync.Mutex
	path      string
	data      map[string]managedConversation
	mode      ConversationCleanupMode
	keepN     int
	maxAge    time.Duration
	whitelist map[string]bool
	persist   *persistStore
}

type conversationPersist struct {
	Conversations map[string]managedConversation `json:"conversations"`
	Whitelist     []string                       `json:"whitelist,omitempty"`
}

func openConversationManager() *conversationManager {
	mode := CleanupAfterResponse
	if v := os.Getenv("M365_CLEANUP_MODE"); v != "" {
		mode = ConversationCleanupMode(v)
	}
	keepN := 5
	if v := os.Getenv("M365_CLEANUP_KEEP_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keepN = n
		}
	}
	maxAge := 24 * time.Hour
	if v := os.Getenv("M365_CLEANUP_MAX_AGE_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			maxAge = time.Duration(h) * time.Hour
		}
	}
	path := os.Getenv("M365_CONVERSATION_CACHE")
	if path == "" {
		path = "conversations.json"
	}
	cm := &conversationManager{
		path:      path,
		data:      map[string]managedConversation{},
		mode:      mode,
		keepN:     keepN,
		maxAge:    maxAge,
		whitelist: map[string]bool{},
	}
	cm.persist = &persistStore{flush: cm.flush}
	cm.loadLocked()
	return cm
}

func (cm *conversationManager) loadLocked() {
	b, err := os.ReadFile(cm.path)
	if err != nil {
		return
	}
	var p conversationPersist
	if json.Unmarshal(b, &p) == nil && p.Conversations != nil {
		cm.data = p.Conversations
		for _, id := range p.Whitelist {
			cm.whitelist[id] = true
		}
		return
	}
	_ = json.Unmarshal(b, &cm.data)
}

// flush 在锁内生成快照，锁外写盘。
func (cm *conversationManager) flush() error {
	cm.mu.Lock()
	p := conversationPersist{Conversations: cm.data}
	for id := range cm.whitelist {
		p.Whitelist = append(p.Whitelist, id)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	cm.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(cm.path, b, 0o600)
}

func (cm *conversationManager) Record(conversationID, accountID, title string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	now := time.Now().UTC()
	cm.data[conversationID] = managedConversation{
		ID:         conversationID,
		AccountID:  accountID,
		CreatedAt:  now,
		LastUsedAt: now,
		Title:      title,
	}
	cm.persist.markDirty()
	log.Printf("[conversation-manager] recorded conversation %s", conversationID)
}

func (cm *conversationManager) Whitelist(conversationID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.whitelist[conversationID] = true
	cm.persist.markDirty()
}

func (cm *conversationManager) Unwhitelist(conversationID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.whitelist, conversationID)
	cm.persist.markDirty()
}

func (cm *conversationManager) IsWhitelisted(conversationID string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.whitelist[conversationID]
}

func (cm *conversationManager) WhitelistedIDs() []string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	out := make([]string, 0, len(cm.whitelist))
	for id := range cm.whitelist {
		out = append(out, id)
	}
	return out
}

// Delete 移除一条对话记录，返回它是否真的存在过。
//
// 之前无论有没有这条记录都照样打印 "deleted conversation <id>" 并标记脏页，
// 日志因此会记下从未发生过的删除，排查时无从分辨。
func (cm *conversationManager) Delete(conversationID string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if _, exists := cm.data[conversationID]; !exists {
		return false
	}
	delete(cm.data, conversationID)
	cm.persist.markDirty()
	log.Printf("[conversation-manager] deleted conversation %s", conversationID)
	return true
}

func (cm *conversationManager) List() []managedConversation {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	out := make([]managedConversation, 0, len(cm.data))
	for _, v := range cm.data {
		out = append(out, v)
	}
	return out
}

func (cm *conversationManager) Cleanup() []string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	var toDelete []string
	now := time.Now().UTC()

	switch cm.mode {
	case CleanupAfterResponse:
		for id, c := range cm.data {
			if cm.whitelist[id] {
				continue
			}
			if now.Sub(c.LastUsedAt) > 30*time.Second {
				toDelete = append(toDelete, id)
			}
		}
	case CleanupMaxAge:
		cutoff := now.Add(-cm.maxAge)
		for id, c := range cm.data {
			if cm.whitelist[id] {
				continue
			}
			if c.CreatedAt.Before(cutoff) {
				toDelete = append(toDelete, id)
			}
		}
	case CleanupKeepN:
		if len(cm.data) > cm.keepN {
			type item struct {
				id       string
				lastUsed time.Time
			}
			items := make([]item, 0, len(cm.data))
			for id, c := range cm.data {
				items = append(items, item{id, c.LastUsedAt})
			}
			sort.Slice(items, func(i, j int) bool {
				return items[i].lastUsed.After(items[j].lastUsed)
			})
			for i := cm.keepN; i < len(items); i++ {
				if cm.whitelist[items[i].id] {
					continue
				}
				toDelete = append(toDelete, items[i].id)
			}
		}
	}

	for _, id := range toDelete {
		delete(cm.data, id)
	}
	if len(toDelete) > 0 {
		cm.persist.markDirty()
		log.Printf("[conversation-manager] cleaned up %d conversations", len(toDelete))
	}
	return toDelete
}

// maxCleanupKeepN 是 keep_n 的上限。云端对话总量本身就该维持在个位数量级，
// 这个上限只是用来把明显是打错的值（负数、上百万）挡在外面。
const maxCleanupKeepN = 1000

// validCleanupMode 判定一个模式字符串是否是已实现的四种之一。
//
// Cleanup 的 switch 对未知模式什么也不做，所以接受任意字符串等于悄悄把自动
// 清理关掉，而接口仍然回 "cleaned"。
func validCleanupMode(mode ConversationCleanupMode) bool {
	switch mode {
	case CleanupAfterResponse, CleanupOnExit, CleanupKeepN, CleanupMaxAge:
		return true
	}
	return false
}

// ShouldCleanup、Mode 都在锁内读 cm.mode：SetMode 是并发写入方（HTTP handler），
// 无锁读取构成数据竞争。
func (cm *conversationManager) ShouldCleanup() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.mode != CleanupOnExit
}

func (cm *conversationManager) Mode() ConversationCleanupMode {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.mode
}

func (cm *conversationManager) SetMode(mode ConversationCleanupMode) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.mode = mode
}

// KeepN 返回 keep_n 模式下保留的对话条数。
func (cm *conversationManager) KeepN() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.keepN
}

// SetKeepN 设置保留条数。非正值被忽略：0 在 Cleanup 里意味着「全部删掉」，
// 不能因为调用方漏传字段就落到那个语义上。
func (cm *conversationManager) SetKeepN(n int) {
	if n < 1 {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.keepN = n
}
