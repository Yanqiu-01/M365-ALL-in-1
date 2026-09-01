package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionBinding 璁板綍涓€娆″唴瀹归敭澶嶇敤鐨勪細璇濄€侷dentity 瀛楁锛圛P/user锛変粎浣?
// 璇婃柇鍏冩暟鎹繚鐣欙紝鍖归厤鍒ゅ畾鍙緷璧栦笂涓嬫枃鍐呭锛岃 Resolve 鐨勫唴瀹归敭閫昏緫銆?
type sessionBinding struct {
	SessionID      string `json:"sessionId"`
	ConversationID string `json:"conversationId"`
	AccountID      string `json:"accountId"`
	// TenantKey 是租户隔离键，取自请求的 API key（extractAPIKey 同源逻辑），
	// 空值视为 "anon"。三级匹配全部按此键过滤，防止跨租户上下文串话。
	TenantKey     string    `json:"tenant_key,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	IPFingerprint string    `json:"ipFingerprint,omitempty"`
	UserField     string    `json:"userField,omitempty"`
	ContextFinger string    `json:"contextFinger,omitempty"`
	// ContextHistory 鎸佷箙鍖栦繚瀛樻渶杩戜竴娆″崗璁殑瀹屾暣娑堟伅锛屼緵閲嶅惎鍚庣户缁仛
	// 鍐呭鍓嶇紑鍖归厤锛岄伩鍏嶈繘绋嬮噸鍚鑷存墍鏈変細璇濋敭鍏ㄩ儴澶辨晥銆?
	ContextHistory []oaiMsg `json:"contextHistory,omitempty"`
	// contentHashes caches a fixed-width SHA-256 digest of role + NUL + text for
	// every message in ContextHistory. Equality is its only use, so retaining the
	// original text here was a duplicate full-history copy with no semantic value.
	contentHashes []string `json:"-"`
	// contextTokens caches the tokenised form of ContextHistory, but only a recent
	// window (24 messages / 64KB by default). Before window capping it stored every
	// distinct token over the WHOLE history, growing unbounded: 100 sessions /
	// 14435 messages retained 108.4MB of tokens, i.e. 440605 distinct tokens, the
	// single largest consumer in this file. contextSimilarityCached needs it, which
	// is a FUZZY FALLBACK heuristic used when strict-prefix matching misses. Cap the
	// window by message count AND by bytes, so the retained size is O(cap) not
	// O(history); scores then reflect "recent turns" instead of "whole history."
	contextTokens map[string]bool `json:"-"`
	// ContextBytes is a cheap size estimate of ContextHistory. It is persisted
	// so compression detection does not have to flatten a large history again
	// after every restart.
	ContextBytes int64 `json:"contextBytes,omitempty"`
}

// compressionEvent is returned by Bind when the same cloud conversation is
// observed with a substantially smaller context. Archive I/O happens outside
// the resolver mutex so a large JSON write never blocks session matching.
type compressionEvent struct {
	SessionID          string
	ConversationID     string
	AccountID          string
	TenantKey          string
	CreatedAt          time.Time
	DetectedAt         time.Time
	BeforeContextBytes int64
	AfterContextBytes  int64
	BeforeMessages     []oaiMsg
	AfterMessages      []oaiMsg
	DroppedMessages    []oaiMsg
}

type sessionResolver struct {
	mu       sync.Mutex
	path     string
	sessions map[string]sessionBinding
	// byExplicit is kept as a small secondary index for callers/tests that
	// construct a resolver directly. The explicit session id is still the
	// canonical key in sessions; this map only avoids a future linear scan.
	byExplicit  map[string]string // explicit X-M365-Session-Id -> sessionID
	byUserField map[string]string // userField -> sessionID
	byIPFinger  map[string]string // ipFingerprint -> sessionID
	byContext   map[string]string // contextFingerprint -> sessionID
	ttl         time.Duration
	contextTTL  time.Duration
	maxSessions int
	// maxContextBytes 是单会话上下文字节上限，0 表示用编译期默认值 —— 直接构造
	// 出来的 resolver（测试、history_capture）不必显式填这个字段就能拿到同样的
	// 保护，与 maxSessions 的处理方式一致。
	maxContextBytes           int64
	compressionThresholdBytes int64
	compressionRatio          float64
	persist                   *persistStore
}

const (
	defaultMaxSessions               = 100
	defaultCompressionThresholdBytes = int64(400 * 1024)
	defaultCompressionRatio          = 0.75
	// 相似度兜底只需要回答「这个请求是不是在续接那个会话」，最近几轮就足以判断。
	// 但 contextTokens 原本对整段历史建词集合，于是每个会话常驻的词表随历史线性
	// 增长 —— 实测 100 个会话 / 14435 条消息留下 440605 个不同词、108MB，是本文件
	// 最大的内存消费者。把取词范围压到历史尾部的一个窗口，词表大小就由窗口上限
	// 决定，与历史长短无关。条数与字节双重设限：条数挡住"很多条小消息"，字节挡住
	// "少数几条超大消息"，任一先到即止。
	defaultSimilarityWindowMessages = 24
	defaultSimilarityWindowBytes    = int64(64 << 10)
	// 单会话上下文字节上限。原本只按会话条数封顶（defaultMaxSessions），于是
	// 少数几个超长会话仍能把常驻字节拉到 GB 级：100 条的额度里，一条 200MB 的
	// 会话和一条 2KB 的会话记一样多。真实样本单会话平均 286KB，所以 4MB 是
	// 病态增长的护栏而不是日常策略 —— 正常会话永远碰不到它。
	defaultMaxSessionContextBytes = int64(4 << 20)
)

// The history/session settings are process environment values. Keep parsing
// deliberately strict: malformed, non-finite, or out-of-range values fall
// back to the compiled default instead of silently weakening a bound.
func boundedPositiveIntEnv(name string, fallback, min, max int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v < min || v > max {
		return fallback
	}
	return v
}

func boundedPositiveInt64Env(name string, fallback, min, max int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || v < min || v > max {
		return fallback
	}
	return v
}

func boundedFloatEnv(name string, fallback, min, max float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max {
		return fallback
	}
	return v
}

func openSessionResolver() *sessionResolver {
	// 闂茬疆 2 灏忔椂鍗宠涓鸿繃鏈燂紙鐢ㄦ埛锛? 灏忔椂涓嶆椿璺冨凡缁忕畻涔咃級銆備細璇濊繃鏈熷悗
	// 浠?sessions.json 鍓旈櫎锛屼簯绔璇濅氦缁?auto_cleanup 鎸夌浉鍚岀獥鍙ｅ洖鏀躲€?
	ttl := 2 * time.Hour
	if v := os.Getenv("M365_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			ttl = d
		}
	}
	contextTTL := 2 * time.Hour
	if v := os.Getenv("M365_CONTEXT_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			contextTTL = d
		}
	}
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = "sessions.json"
	}
	maxSessions := boundedPositiveIntEnv("M365_SESSION_MAX", defaultMaxSessions, 1, 10000)
	// 下界 64KB：再低就会把正常长度的会话也赶走，那不是护栏而是功能损坏。
	maxContextBytes := boundedPositiveInt64Env("M365_SESSION_MAX_CONTEXT_BYTES", defaultMaxSessionContextBytes, 64<<10, 512<<20)
	compressionThreshold := boundedPositiveInt64Env("M365_HISTORY_COMPRESSION_THRESHOLD_BYTES", defaultCompressionThresholdBytes, 1024, 100<<20)
	compressionRatio := boundedFloatEnv("M365_HISTORY_COMPRESSION_RATIO", defaultCompressionRatio, 0.05, 0.99)
	sr := &sessionResolver{
		path:                      path,
		sessions:                  map[string]sessionBinding{},
		byExplicit:                map[string]string{},
		byUserField:               map[string]string{},
		byIPFinger:                map[string]string{},
		byContext:                 map[string]string{},
		ttl:                       ttl,
		contextTTL:                contextTTL,
		maxSessions:               maxSessions,
		maxContextBytes:           maxContextBytes,
		compressionThresholdBytes: compressionThreshold,
		compressionRatio:          compressionRatio,
	}
	sr.persist = &persistStore{flush: sr.flush}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	if b, err := os.ReadFile(sr.path); err == nil {
		var list []sessionBinding
		if err := json.Unmarshal(b, &list); err == nil {
			now := time.Now().UTC()
			for _, s := range list {
				if now.Sub(s.LastUsedAt) > sr.ttl {
					continue
				}
				// 旧 sessions.json 无 tenant_key：读出为空串，视为 "anon"，
				// 不崩溃；该会话只会被同属 "anon"（即无 API key）的请求复用。
				if s.TenantKey == "" {
					s.TenantKey = "anon"
				}
				// contentHashes is json:"-", so a session read back from disk has
				// no cache and every comparison would fall back to re-running
				// contentToString over its whole history. That is exactly the
				// per-request re-materialisation that saturated the CPU, and it
				// returned on every restart. Rebuild the cache once here.
				s.contentHashes = computeContentDigests(s.ContextHistory)
				s.contextTokens = messageTokenSet(s.ContextHistory)
				if s.ContextBytes <= 0 {
					s.ContextBytes = estimateContextBytes(s.ContextHistory, computeContentHashes(s.ContextHistory))
				}
				sr.reindexLocked(s)
			}
			before := len(sr.sessions)
			sr.evictLocked()
			if len(sr.sessions) < before && sr.persist != nil {
				sr.persist.markDirty()
			}
		}
	}
}

// flush 在锁内生成快照，锁外写盘。
func (sr *sessionResolver) flush() error {
	sr.mu.Lock()
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	sr.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(sr.path, b, 0o600)
}

func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	// 不能截断 ContextHistory 后继续把截断长度当 HistoryLen 返回：调用方会按
	// body.Messages[HistoryLen:] 发增量，而截断后的尾部不是请求的前缀，必然丢失
	// 或重复上下文。超限会话因此直接不参加后续增量复用；下一次请求走全量，而
	// 不是猜一个不可靠的边界。ContextHistory 从未被静默裁剪。
	maxContextBytes := sr.maxContextBytes
	if maxContextBytes <= 0 {
		maxContextBytes = defaultMaxSessionContextBytes
	}
	if s.ContextBytes > maxContextBytes {
		if old, ok := sr.sessions[s.SessionID]; ok {
			sr.dropLocked(s.SessionID, old)
		}
		return
	}

	// Bind 会在原 sessionID 上更新 IP/user/context 指纹。先删掉这个 ID 旧有的
	// 索引项，才能保证索引是当前值的反向映射；否则每次网络变化都留下一个永不
	// 删除的旧 key，既泄漏内存，又会让诊断索引指向已经失效的指纹。
	if old, ok := sr.sessions[s.SessionID]; ok {
		sr.deleteIndexesLocked(s.SessionID, old)
	}
	sr.sessions[s.SessionID] = s
	if sr.byExplicit == nil {
		sr.byExplicit = map[string]string{}
	}
	if sr.byUserField == nil {
		sr.byUserField = map[string]string{}
	}
	if sr.byIPFinger == nil {
		sr.byIPFinger = map[string]string{}
	}
	if sr.byContext == nil {
		sr.byContext = map[string]string{}
	}
	if s.SessionID != "" {
		sr.byExplicit[s.SessionID] = s.SessionID
	}
	if s.UserField != "" {
		sr.byUserField[s.UserField] = s.SessionID
	}
	if s.IPFingerprint != "" {
		sr.byIPFinger[s.IPFingerprint] = s.SessionID
	}
	if s.ContextFinger != "" {
		sr.byContext[s.ContextFinger] = s.SessionID
	}
}

// deleteIndexesLocked removes only entries that still point to id. Another
// session may have since claimed the same diagnostic key, so unconditional
// deletion would corrupt that newer reverse mapping.
func (sr *sessionResolver) deleteIndexesLocked(id string, s sessionBinding) {
	if sr.byExplicit[s.SessionID] == id {
		delete(sr.byExplicit, s.SessionID)
	}
	if sr.byUserField[s.UserField] == id {
		delete(sr.byUserField, s.UserField)
	}
	if sr.byIPFinger[s.IPFingerprint] == id {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if sr.byContext[s.ContextFinger] == id {
		delete(sr.byContext, s.ContextFinger)
	}
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			sr.dropLocked(id, s)
		}
	}
	if sr.maxSessions < 1 {
		sr.maxSessions = defaultMaxSessions
	}
	if len(sr.sessions) > sr.maxSessions {
		// Bound memory by dropping the least recently used sessions.
		ids := make([]string, 0, len(sr.sessions))
		last := make(map[string]time.Time, len(sr.sessions))
		for id, s := range sr.sessions {
			ids = append(ids, id)
			last[id] = s.LastUsedAt
		}
		sort.Slice(ids, func(i, j int) bool { return last[ids[i]].Before(last[ids[j]]) })
		for _, id := range ids[:len(sr.sessions)-sr.maxSessions] {
			sr.dropLocked(id, sr.sessions[id])
		}
	}
}

func (sr *sessionResolver) dropLocked(id string, s sessionBinding) {
	delete(sr.sessions, id)
	if sr.byExplicit[s.SessionID] == id {
		delete(sr.byExplicit, s.SessionID)
	}
	if sr.byUserField[s.UserField] == id {
		delete(sr.byUserField, s.UserField)
	}
	if sr.byIPFinger[s.IPFingerprint] == id {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if sr.byContext[s.ContextFinger] == id {
		delete(sr.byContext, s.ContextFinger)
	}
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
	// HistoryLen 鏄鐢ㄥ懡涓椂"浜戠瀵硅瘽宸插寘鍚殑娑堟伅鏉℃暟"锛?
	// 鍗冲閲忓彂閫佺殑璧风偣涓嬫爣锛坆ody.Messages[HistoryLen:] 鍙彂鏂板閮ㄥ垎锛夈€?
	HistoryLen int
}

func clientIPFingerprint(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ua := r.Header.Get("User-Agent")
	data := host + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextFingerprint(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	var parts []string
	limit := len(messages)
	if limit > 3 {
		limit = 3
	}
	for i := len(messages) - limit; i < len(messages); i++ {
		m := messages[i]
		parts = append(parts, m.Role+":"+contentToString(m.Content))
	}
	data := strings.Join(parts, "||")
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

// contextSimilarity 计算两组消息的上下文相似度，用于严格前缀匹配失败后的
// 弱约束兜底。APK 证据：session_resolver.go:202-215（14 行），函数体内联了
// strings.Builder 的 WriteString/String，即两侧各拼成一段文本后比较词集合。
func contextSimilarity(hist, msgs []oaiMsg) float64 {
	if len(hist) == 0 || len(msgs) == 0 {
		return 0
	}
	return jaccardSets(messageTokenSet(hist), messageTokenSet(msgs))
}

// messageTokenSet tokenises a recent suffix rather than flattening the complete
// history. Similarity is only a fallback after strict-prefix matching failed, so
// the useful signal is whether the newest turns continue each other; holding an
// unbounded vocabulary for old turns buys little and made every stored session
// scale with lifetime conversation text. Both caps are environment-overridable
// for installations with an unusual turn shape, using the same strict parsing
// policy as the other M365_SESSION_* bounds.
func messageTokenSet(msgs []oaiMsg) map[string]bool {
	if len(msgs) == 0 {
		return nil
	}
	maxMessages := boundedPositiveIntEnv("M365_SESSION_SIMILARITY_WINDOW_MESSAGES", defaultSimilarityWindowMessages, 1, 10000)
	maxBytes := boundedPositiveInt64Env("M365_SESSION_SIMILARITY_WINDOW_BYTES", defaultSimilarityWindowBytes, 1024, 16<<20)

	// Walk from the newest message backwards, then write forward again. A suffix
	// gives a new request's latest user turn the same weight as the stored one;
	// writing forward preserves ordinary role/content token boundaries.
	start := len(msgs)
	var used int64
	for start > 0 && len(msgs)-start < maxMessages {
		m := msgs[start-1]
		messageBytes := int64(len(m.Role) + 1 + len(contentToString(m.Content)))
		if used+messageBytes > maxBytes {
			break
		}
		used += messageBytes
		start--
	}
	// Always admit the last message, but only its tail up to maxBytes. An oversized
	// final turn must not turn the set into nil (which disables the fallback), nor
	// may a single pasted log evade the byte cap.
	if start == len(msgs) {
		start--
	}

	var text strings.Builder
	text.Grow(int(used))
	for _, m := range msgs[start:] {
		remaining := maxBytes - int64(text.Len())
		if remaining <= 1 { // reserve one byte for the separator below
			break
		}
		prefix := m.Role + ":"
		if int64(len(prefix))+1 > remaining {
			break
		}
		text.WriteString(prefix)
		content := contentToString(m.Content)
		contentBudget := remaining - int64(len(prefix)) - 1
		if int64(len(content)) > contentBudget {
			// Keep the end of an oversized message: it contains the newest natural-
			// language clause, and slicing bytes is safe because strings.Fields will
			// simply treat any incomplete UTF-8 rune as a separator/non-word byte.
			content = content[len(content)-int(contentBudget):]
		}
		text.WriteString(content)
		text.WriteByte('\n')
	}
	tokens := tokenize(text.String())
	set := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		set[token] = true
	}
	return set
}

// contextSimilarityCached is contextSimilarity with the session side already
// tokenised. It falls back to the uncached path when the cache is absent.
func contextSimilarityCached(hist []oaiMsg, histTokens map[string]bool, msgs []oaiMsg, msgTokens map[string]bool) float64 {
	if len(hist) == 0 || len(msgs) == 0 {
		return 0
	}
	if histTokens == nil || msgTokens == nil {
		return contextSimilarity(hist, msgs)
	}
	return jaccardSets(histTokens, msgTokens)
}

// jaccardSets is jaccardSimilarity over prebuilt sets, so the caller can reuse
// them across sessions rather than allocating two maps per comparison.
func jaccardSets(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	intersection := 0
	for token := range small {
		if large[token] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// jaccardSimilarity 返回两个词集合的 Jaccard 系数 |A∩B| / |A∪B|。
// APK 证据：session_resolver.go:218-237（20 行），无内联，纯集合运算。
func jaccardSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, token := range a {
		setA[token] = true
	}
	setB := make(map[string]bool, len(b))
	for _, token := range b {
		setB[token] = true
	}
	intersection := 0
	for token := range setA {
		if setB[token] {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// tokenize 按空白切分并归一化为小写词序列。
// APK 证据：session_resolver.go:240-246（7 行、176 字节）。
func tokenize(text string) []string {
	fields := strings.Fields(strings.ToLower(text))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, field)
	}
	return out
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	// 租户键：取 Authorization Bearer / X-API-Key 前缀，与 server.extractAPIKey
	// 同源逻辑（同包，本文件实现最小等价函数 requestTenantKey，避免跨文件耦合
	// 与循环依赖）。空则回退 "anon"。三级匹配均按此键过滤，防跨租户串话。
	tenantKey := requestTenantKey(r)

	explicitID := r.Header.Get("X-M365-Session-Id")

	// 瀹㈡埛绔樉寮忔寚瀹氱殑浼氳瘽 ID 鏄渶楂樹紭鍏堢殑缁帴璇箟锛氫笉鍙備笌浠讳綍韬唤鍒ゅ畾锛?
	// 鐢辫皟鐢ㄦ柟涓诲姩鍐冲畾瑕佺户缁摢涓簯绔璇濄€?
	if explicitID != "" {
		if sess, ok := sr.sessions[explicitID]; ok && sess.TenantKey == tenantKey {
			sess.LastUsedAt = time.Now().UTC()
			sr.sessions[explicitID] = sess
			sr.persist.markDirty()
			return ResolveResult{
				SessionID:      sess.SessionID,
				ConversationID: sess.ConversationID,
				AccountID:      sess.AccountID,
				MatchedBy:      "explicit",
				IsNew:          false,
				HistoryLen:     len(sess.ContextHistory),
			}
		}
	}

	// 鍐呭閿細鍗忚娑堟伅鍚嶅簭鍒椾弗鏍肩瓑浜庢煇涓凡璁板綍浼氳瘽鐨勫巻鍙叉椂鐩存帴澶嶇敤杩欎釜
	// 浜戠瀵硅瘽锛屼絾鍙湪鍚屼竴 IP/UA 鎸囩汗涓嬶紝閬垮厤鐭秷鎭湪涓嶅悓鐢ㄦ埛闂翠簰绔?
	// HistoryLen 杩斿洖璇ュ墠缂€闀垮害锛屼笂灞傛嵁姝ゅ彧鍙戦€?messages[HistoryLen:] 澧為噺銆?
	ipFinger := clientIPFingerprint(r)
	if bestID, n := sr.matchContextLocked(ipFinger, tenantKey, body.Messages); bestID != "" {
		sess := sr.sessions[bestID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_prefix_%d", n),
			IsNew:          false,
			HistoryLen:     n,
		}
	}

	// 弱约束兜底：内容不构成严格前缀，但与某个历史高度相似（如客户端本地
	// 截断了历史）时仍复用该会话。此时增量边界未知，HistoryLen 归零，
	// 由上层发送全量。
	//
	// APK 证据（tools/apktool strings + 反汇编）：
	//   Resolve 体内字符串 M365_CONTEXT_SIMILARITY / context_prefix_%d /
	//   context_similar_%.2f，三者均在 (*sessionResolver).Resolve 内；
	//   该函数 inline tree 为空，即筛选逻辑直接写在 Resolve 体内，
	//   APK 中不存在独立的 matchSimilarLocked 方法（函数表 246 与 249
	//   行段间无空隙）。
	// 阈值：环境变量优先，默认 0.6，上界 1.0，NaN/越界回退默认。
	// APK +0x029c ADRP+ADD 取 "M365_CONTEXT_SIMILARITY"（MOVZ x1,#23），
	// 空或解析失败时经 CBNZ 落到 +0x02b0 ADRP x27,0x5be000 /
	// LDR d0,[x27,#1232]，即 0x5be4d0 = 0x3fe3333333333333 = 0.6；
	// +0x02c8 FCMP d0,d0 排 NaN，+0x02d0 FMOV d1,#1.0 与 +0x02d4
	// FCMP d0,d1 / B.LS 限定上界。
	threshold := 0.6
	if raw := strings.TrimSpace(os.Getenv("M365_CONTEXT_SIMILARITY")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && v > 0 && v <= 1 {
			threshold = v
		}
	}
	bestSimilar := ""
	bestScore := 0.0
	var bestSimilarAt time.Time
	// Tokenise the request once. Previously this happened inside the loop for
	// both sides, so the cost scaled with sessions x history bytes per request.
	requestTokens := messageTokenSet(body.Messages)
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		// 租户过滤：相似度兜底只在同租户候选间扫描，跨租户上下文不得互串。
		if sess.TenantKey != tenantKey {
			continue
		}
		score := contextSimilarityCached(sess.ContextHistory, sess.contextTokens, body.Messages, requestTokens)
		if score < threshold {
			continue
		}
		if score > bestScore || (score == bestScore && sess.LastUsedAt.After(bestSimilarAt)) {
			bestSimilar, bestScore, bestSimilarAt = id, score, sess.LastUsedAt
		}
	}
	if bestSimilar != "" {
		sess := sr.sessions[bestSimilar]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestSimilar] = sess
		sr.persist.markDirty()
		// 内容只是相似而非严格前缀，增量边界未知，交由上层发送全量。
		//
		// 但有一种情形必须排除在复用之外：整批消息被原样重发。客户端重试、
		// 用户把同一句话再问一遍都会命中这里 —— Jaccard 对「历史多一条模型
		// 回复」这种差异给 0.67，稳定越过 0.6 阈值。若复用该会话再发一遍全量，
		// 上游会在同一个云端对话里第二次看到已经答过的内容，返回空补全，
		// 客户端收到的就是空回复。原样重发应当开一轮新对话。
		if repeatSuffixLen(sess.ContextHistory, body.Messages, sess.contentHashes, nil) > 0 {
			return ResolveResult{IsNew: true}
		}
		// 相似度是弱证据，不足以作为账号切换依据：不得采纳命中会话的
		// AccountID，只保留当前请求自身解析得到的账号。仅当本请求未带账号
		// 时才沿用会话记录里的 AccountID，避免把别人的账号贴到当前用户上。
		resolvedAccountID := body.AccountID
		if resolvedAccountID == "" {
			resolvedAccountID = sess.AccountID
		}
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      resolvedAccountID,
			MatchedBy:      fmt.Sprintf("context_similar_%.2f", bestScore),
			IsNew:          false,
			HistoryLen:     0,
		}
	}

	return ResolveResult{IsNew: true}
}

// matchContextLocked 浠庡叏閮ㄤ細璇濅腑鎵惧埌鍏?contextHistory 涓ユ牸浣滀负娑堟伅鍓嶇紑鐨?
// 閭ｄ釜浼氳瘽锛涘彧閫夊墠缂€鏈€闀跨殑涓€涓紝閬垮厤鐭墠缂€鍦ㄤ笉鍚屼細璇濋棿浜掓挒銆傝繑鍥?
// (sessionID, 鍖归厤鍒扮殑娑堟伅鏉℃暟)銆?
func (sr *sessionResolver) matchContextLocked(ipFinger, tenantKey string, messages []oaiMsg) (string, int) {
	if len(messages) == 0 {
		return "", 0
	}
	msgHashes := requestContentHashes(messages)
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		// 租户过滤：内容前缀匹配同样只在同租户候选间进行。
		if sess.TenantKey != tenantKey {
			continue
		}
		n := contextPrefixLenHashed(sess.ContextHistory, sess.contentHashes, messages, msgHashes)
		if n >= 1 && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

// contextPrefixLenHashed is contextPrefixLen with precomputed content strings.
// It compares role + cached string instead of calling contentToString per
// message. Falls back to the uncached version when either side has no cache.
func contextPrefixLenHashed(hist []oaiMsg, histHashes []string, msgs []oaiMsg, msgHashes []string) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	if len(histHashes) != len(hist) || len(msgHashes) != len(msgs) {
		return contextPrefixLen(hist, msgs)
	}
	for i := range hist {
		if !messagesEqualHashed(hist[i], histHashes[i], msgs[i], msgHashes[i]) {
			return 0
		}
	}
	return len(hist)
}

// messagesEqualHashed is messagesEqual with the contentToString already done.
// The hash covers role + text content; tool_calls are still compared structurally.
func messagesEqualHashed(a oaiMsg, aHash string, b oaiMsg, bHash string) bool {
	if a.Role != b.Role {
		return false
	}
	if aHash != bHash {
		return false
	}
	if (a.ToolCalls == nil) != (b.ToolCalls == nil) {
		return false
	}
	for i := range a.ToolCalls {
		if i >= len(b.ToolCalls) {
			return false
		}
		if toolCallEqual(a.ToolCalls[i], b.ToolCalls[i]) {
			continue
		}
		return false
	}
	return len(a.ToolCalls) == len(b.ToolCalls)
}

// repeatSuffixLen 处理「同一批消息被重发」的情形：hist 是上一轮协商的完整
// 消息（含模型回复），msgs 是这次到达的消息。若 msgs 逐条等于 hist 去掉尾部
// assistant 回复后的那一段，说明客户端把同样的请求又发了一次，返回 len(msgs)
// 作为增量起点 —— 也就是「已经全部发过，本轮没有新内容」。
//
// 返回 len(msgs) 而非 0 让上层跳过重复正文；上层随后会发现增量为空并保留
// 原 prompt，但对话已定位到正确的会话，不会把旧内容再灌一遍。
func repeatSuffixLen(hist, msgs []oaiMsg, histHashes, msgHashes []string) int {
	if len(hist) == 0 || len(msgs) == 0 || len(msgs) > len(hist) {
		return 0
	}
	for i := range msgs {
		if !messagesEqualCached(hist[i], msgs[i], i, histHashes, msgHashes) {
			return 0
		}
	}
	// 仅当 hist 多出来的部分全是模型侧输出时才算「重发」，否则是别的会话形状。
	for _, extra := range hist[len(msgs):] {
		if extra.Role != "assistant" && extra.Role != "tool" {
			return 0
		}
	}
	return len(msgs)
}

// messagesEqualCached falls back to the uncached path when either hash slice
// is missing or shorter than the message index. The hot path (matchContext)
// always has both caches populated by computeContentDigests.
func messagesEqualCached(a, b oaiMsg, idx int, aHashes, bHashes []string) bool {
	if aHashes != nil && bHashes != nil && idx < len(aHashes) && idx < len(bHashes) {
		// The hash only covers role + text. Comparing hashes alone treated two
		// assistant turns with different tool_calls as the same message, which
		// let an unrelated turn join an existing conversation. Delegate to the
		// hashed comparison so tool_calls are still checked structurally.
		return messagesEqualHashed(a, aHashes[idx], b, bHashes[idx])
	}
	return messagesEqual(a, b)
}

// contextPrefixLen 杩斿洖 hist 鏄惁涓ユ牸鏄?msgs 鐨勫墠缂€銆俬ist 涓虹┖鎴栦笉鏄墠缂€
// 鏃惰繑鍥?0锛涘懡涓椂杩斿洖 len(hist)锛屽嵆澧為噺鍙戦€佽捣鐐广€?
func contextPrefixLen(hist, msgs []oaiMsg) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	for i := range hist {
		if !messagesEqual(hist[i], msgs[i]) {
			return 0
		}
	}
	return len(hist)
}

// messagesEqual 鍒ゅ畾涓ゆ潯娑堟伅鍦ㄤ細璇濋敭鎰忎箟涓婄瓑浠凤細role 涓庢枃鏈唴瀹逛竴鑷淬€?
// 蹇界暐 tool_calls 鐨?ID 缁嗚妭锛堜細璇濋敭鍙叧蹇冨唴瀹瑰浣曡妯″瀷娑堝寲锛夈€?
func messagesEqual(a, b oaiMsg) bool {
	if a.Role != b.Role {
		return false
	}
	ta := contentToString(a.Content)
	tb := contentToString(b.Content)
	if ta != tb {
		return false
	}
	if (a.ToolCalls == nil) != (b.ToolCalls == nil) {
		return false
	}
	for i := range a.ToolCalls {
		if i >= len(b.ToolCalls) {
			return false
		}
		if toolCallEqual(a.ToolCalls[i], b.ToolCalls[i]) {
			continue
		}
		return false
	}
	return len(a.ToolCalls) == len(b.ToolCalls)
}

// toolCallEqual 比较 name 与 arguments，忽略 ID：同一段工具调用重放时
// ID 由客户端重新生成，不应影响会话键。
func toolCallEqual(x, y map[string]any) bool {
	xFunc, _ := x["function"].(map[string]any)
	yFunc, _ := y["function"].(map[string]any)
	xn, _ := xFunc["name"].(string)
	yn, _ := yFunc["name"].(string)
	if xn != yn {
		return false
	}
	xa, _ := xFunc["arguments"].(string)
	ya, _ := yFunc["arguments"].(string)
	return xa == ya
}

// computeContentHashes returns the role + NUL + contentToString of each message.
// Despite the name it does not hash: it is the full content key, and its LENGTH
// is what estimateContextBytes uses as the per-message size proxy. Keep it that
// way — history_archive.go depends on that length for compression detection, so
// switching this to a fixed-width digest would make ContextBytes proportional to
// message count instead of bytes and silently disable shrink detection.
//
// 因此这个切片只用于「算大小」，是一次性中间产物，绝不长期持有：常驻缓存存的是
// 下面 computeContentDigests 的定长摘要。
func computeContentHashes(msgs []oaiMsg) []string {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + "\x00" + contentToString(m.Content)
	}
	return out
}

// computeContentDigests 返回每条消息内容键的定长摘要，供 contentHashes 常驻缓存。
//
// 原实现把 role+NUL+正文原样存下来，等于给每条消息的正文留了第二份完整副本 ——
// pprof 归因 computeContentHashes 75.91MB、复现负载里 contentHashes 常驻 23.3MB，
// 而这份副本的全部用途只是字符串「相等比较」（contextPrefixLenHashed /
// repeatSuffixLen / matchContextLocked）。摘要对相等比较是等价的：同内容同摘要，
// 异内容异摘要，于是常驻量从"正文大小"塌缩成"消息条数 x 64 字节"。
//
// 这里不像 clientIPFingerprint 那样截断到 16 字节。指纹只是索引，撞了还有后续
// 校验兜底；而内容相等直接决定"要不要复用这个云端对话"，一次碰撞就是把两段无关
// 对话接在一起。留全 256 位，代价是每条消息多 32 字节，换掉这个风险很划算。
func computeContentDigests(msgs []oaiMsg) []string {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = digestContentKey(m.Role + "\x00" + contentToString(m.Content))
	}
	return out
}

// digestContentKeys digests an already-built content key slice. Bind needs the
// keys anyway to size the context, so reusing them avoids a second pass over
// every message body.
func digestContentKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = digestContentKey(key)
	}
	return out
}

func digestContentKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// requestContentHashes is the per-request analogue, computed once before the
// resolver loop so matchContextLocked shares the work across candidate sessions.
// It must produce the SAME flavour as the retained contentHashes — comparing a
// digest against a raw content key would never be equal and would quietly turn
// every strict-prefix match into a new session.
func requestContentHashes(msgs []oaiMsg) []string { return computeContentDigests(msgs) }

// requestTenantKey 提取请求的租户键，与 server.extractAPIKey 同源：
// 优先 X-API-Key，其次 Authorization Bearer，超过 8 字符取前缀加省略号，
// 空则回退 "anon"。本文件不能 import server 包（同包），此处实现最小等价
// 函数以避免跨文件耦合；server.extractAPIKey 仍是用量记账的权威来源。
func requestTenantKey(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if key != "" {
		return key
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		key = strings.TrimSpace(auth[7:])
	}
	if len(key) > 8 {
		return key[:8] + "..."
	}
	if key == "" {
		return "anon"
	}
	return key
}

func (sr *sessionResolver) Bind(sessionID, conversationID, accountID string, body *oaiReq, assistantText string, r *http.Request) *compressionEvent {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	now := time.Now().UTC()
	tenantKey := requestTenantKey(r)
	history := cloneMessages(body.Messages)
	if strings.TrimSpace(assistantText) != "" {
		history = append(history, oaiMsg{Role: "assistant", Content: assistantText})
	}
	hashes := computeContentHashes(history)
	digests := digestContentKeys(hashes)
	contextBytes := estimateContextBytes(history, hashes)
	explicitID := r.Header.Get("X-M365-Session-Id")
	if explicitID != "" && sessionID == "" {
		sessionID = explicitID
	}
	// 同一云端对话只保留一条记录：内容键命中后增量轮次更新已存在会话，
	// 而不是每次 Bind 都新建一条，避免 sessions.json 膨胀。
	if sessionID != "" {
		if sess, ok := sr.sessions[sessionID]; ok {
			var compression *compressionEvent
			if sess.ConversationID == conversationID && sess.TenantKey == tenantKey {
				compression = compressionEventFor(sess, history, contextBytes, now, sr.compressionThresholdBytes, sr.compressionRatio)
			}
			sess.ConversationID = conversationID
			sess.AccountID = accountID
			sess.TenantKey = tenantKey
			sess.LastUsedAt = now
			sess.UserField = body.User
			sess.IPFingerprint = clientIPFingerprint(r)
			sess.ContextFinger = contextFingerprint(history)
			sess.ContextHistory = history
			sess.ContextBytes = contextBytes
			sess.contentHashes = digests
			sess.contextTokens = messageTokenSet(history)
			// 不再先写 map 再 reindex：reindexLocked 要读到"更新前"的绑定才能删掉
			// 被取代的旧指纹索引，提前赋值会让它读到新值而漏删。sessions 的 key
			// 恒等于 SessionID，reindexLocked 写的是同一格，语义不变。
			sr.reindexLocked(sess)
			sr.persist.markDirty()
			return compression
		}
	}
	// A cloud conversation can retain its ConversationID while ChatHub issues a
	// new SessionID. Preserve the existing per-session binding semantics, but
	// still use the most recent same-conversation context as the compression
	// baseline before creating the new binding.
	var pendingCompression *compressionEvent
	if sessionID != "" && conversationID != "" {
		var candidate sessionBinding
		found := false
		for _, sess := range sr.sessions {
			if sess.ConversationID != conversationID || sess.TenantKey != tenantKey {
				continue
			}
			if !found || sess.LastUsedAt.After(candidate.LastUsedAt) {
				candidate, found = sess, true
			}
		}
		if found {
			pendingCompression = compressionEventFor(candidate, history, contextBytes, now, sr.compressionThresholdBytes, sr.compressionRatio)
			if pendingCompression != nil {
				pendingCompression.SessionID = sessionID
			}
		}
	}
	if sessionID == "" {
		for _, sess := range sr.sessions {
			if sess.ConversationID == conversationID && sess.TenantKey == tenantKey {
				compression := compressionEventFor(sess, history, contextBytes, now, sr.compressionThresholdBytes, sr.compressionRatio)
				sess.LastUsedAt = now
				sess.AccountID = accountID
				sess.TenantKey = tenantKey
				sess.UserField = body.User
				sess.IPFingerprint = clientIPFingerprint(r)
				sess.ContextFinger = contextFingerprint(history)
				sess.ContextHistory = history
				sess.ContextBytes = contextBytes
				sess.contentHashes = digests
				sess.contextTokens = messageTokenSet(history)
				sr.reindexLocked(sess)
				sr.persist.markDirty()
				return compression
			}
		}
		sessionID = uuid.NewString()
	}

	sess := sessionBinding{
		SessionID:      sessionID,
		ConversationID: conversationID,
		AccountID:      accountID,
		TenantKey:      tenantKey,
		CreatedAt:      now,
		LastUsedAt:     now,
		IPFingerprint:  clientIPFingerprint(r),
		UserField:      body.User,
		ContextFinger:  contextFingerprint(history),
		ContextHistory: history,
		ContextBytes:   contextBytes,
	}
	sess.contentHashes = digests
	sess.contextTokens = messageTokenSet(history)

	sr.reindexLocked(sess)
	sr.persist.markDirty()
	return pendingCompression
}

// SetMaxSessions changes the live-context ceiling without requiring a restart.
// Lowering the ceiling immediately removes the least recently used entries and
// synchronously flushes sessions.json so a restart cannot resurrect them.
func (sr *sessionResolver) SetMaxSessions(max int) (evicted int) {
	if sr == nil || max < 1 || max > 10000 {
		return 0
	}
	sr.mu.Lock()
	if sr.maxSessions == max {
		sr.mu.Unlock()
		return 0
	}
	before := len(sr.sessions)
	sr.maxSessions = max
	sr.evictLocked()
	evicted = before - len(sr.sessions)
	persist := sr.persist
	if evicted > 0 && persist != nil {
		persist.markDirty()
	}
	sr.mu.Unlock()
	if evicted > 0 && persist != nil {
		if err := persist.flushNowBlocking(); err != nil {
			log.Printf("[session-resolver] persist after max-session eviction failed: %v", err)
		}
	}
	return evicted
}

func (sr *sessionResolver) MaxSessions() int {
	if sr == nil {
		return defaultMaxSessions
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.maxSessions < 1 {
		return defaultMaxSessions
	}
	return sr.maxSessions
}

func (sr *sessionResolver) GetSession(sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	return s, ok
}

func (sr *sessionResolver) GetConversation(conversationID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, session := range sr.sessions {
		if session.ConversationID == conversationID {
			session.ContextHistory = cloneMessages(session.ContextHistory)
			return session, true
		}
	}
	return sessionBinding{}, false
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

func (sr *sessionResolver) DeleteSession(sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	if !ok {
		return false
	}
	sr.dropLocked(sessionID, s)
	sr.persist.markDirty()
	return true
}

// ReassignAccount repoints one session at a different account, keeping the
// conversation history so the next turn continues rather than starting over.
// The cloud ConversationID is cleared on purpose: a conversation belongs to the
// account that created it, so replaying it under a new account is exactly the
// CrossID mixing the resolver exists to prevent. The next request therefore
// opens a fresh upstream conversation while the local history is still replayed.
func (sr *sessionResolver) ReassignAccount(sessionID, accountID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sess, ok := sr.sessions[sessionID]
	if !ok {
		return false
	}
	sess.AccountID = accountID
	sess.ConversationID = ""
	sess.LastUsedAt = time.Now().UTC()
	sr.reindexLocked(sess)
	sr.persist.markDirty()
	return true
}

// SessionsForConversation lists the session ids bound to a cloud conversation.
// The dashboard addresses a row by conversation, while reassignment operates on
// the session, so the handler needs this translation.
func (sr *sessionResolver) SessionsForConversation(conversationID string) []string {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := []string{}
	for sid, sess := range sr.sessions {
		if sess.ConversationID == conversationID {
			out = append(out, sid)
		}
	}
	sort.Strings(out)
	return out
}

// UnbindByConversation drops every session bound to the given conversation.
// Called after an automatic cleanup deletes the cloud conversation, so the
// anti-CrossID resolver never reuses a dead conversation.
func (sr *sessionResolver) UnbindByConversation(conversationID string) int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	removed := 0
	for sid, s := range sr.sessions {
		if s.ConversationID != conversationID {
			continue
		}
		sr.dropLocked(sid, s)
		removed++
	}
	if removed > 0 {
		sr.persist.markDirty()
	}
	return removed
}

func cloneMessages(msgs []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, len(msgs))
	copy(out, msgs)
	return out
}
