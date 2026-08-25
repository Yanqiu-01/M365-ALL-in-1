package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDiagMaxBytes int64 = 4 << 20

var (
	diagOnce sync.Once
	diagFile *os.File
	diagMu   sync.Mutex
	inflight sync.Map // map[string]inflightRequest

	chatSlotsMu sync.Mutex
	chatSlots   chan struct{}
	chatSlotN   int
	// 原版 /api/live 的 chat 对象含 peak/rejected/total 累计量，
	// 只看 channel 长度无法还原，需要独立计数。
	chatSlotPeak     int
	chatSlotRejected uint64
	chatSlotTotal    uint64
)

type inflightRequest struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"`
	Method  string    `json:"method"`
	Started time.Time `json:"started"`
	Stage   string    `json:"stage,omitempty"`
}

// diagPath is recovered from the APK's fixed path components.
func diagPath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "server-stages.log")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "m365-copilot2apid", "server-stages.log")
}

func diagEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("M365_STAGE_LOG"))) {
	case "", "0", "no", "off", "false":
		return false
	default:
		return true
	}
}

func diagWriter() *os.File {
	if !diagEnabled() {
		return nil
	}
	diagOnce.Do(func() {
		path := diagPath()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			diagFile = file
		}
	})
	return diagFile
}

// closeDiagWriter releases the stage-log handle and rearms diagOnce so the next
// diagWriter call reopens at the current diagPath().
//
// Production never calls this: the handle is meant to live for the process. It
// exists for tests, which point M365_STAGE_LOG at t.TempDir() -- on Windows the
// directory cannot be removed while the file is open, so the cleanup step
// failed the test even though its assertions had passed.
func closeDiagWriter() {
	if diagFile != nil {
		_ = diagFile.Close()
		diagFile = nil
	}
	diagOnce = sync.Once{}
}
func diagMaxBytes() int64 {
	value := strings.TrimSpace(os.Getenv("M365_STAGE_LOG_MAX_BYTES"))
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
		return parsed
	}
	return defaultDiagMaxBytes
}

// stage writes a bounded JSONL event. Fields are diagnostics only; callers
// must not pass tokens, authorization headers, or raw request bodies.
func stage(requestID, name string, fields map[string]any) {
	entry := map[string]any{
		"at":    time.Now().UTC().Format(time.RFC3339Nano),
		"id":    requestID,
		"stage": name,
	}
	if value, ok := inflight.Load(requestID); ok {
		if request, ok := value.(inflightRequest); ok {
			entry["elapsed_ms"] = time.Since(request.Started).Milliseconds()
		}
	}
	for key, value := range fields {
		entry[key] = value
	}
	if writer := diagWriter(); writer != nil {
		line, err := json.Marshal(entry)
		if err == nil {
			diagMu.Lock()
			if info, statErr := writer.Stat(); statErr == nil && info.Size()+int64(len(line)+1) > diagMaxBytes() {
				_ = writer.Truncate(0)
				_, _ = writer.Seek(0, 0)
			}
			_, _ = writer.Write(append(line, '\n'))
			diagMu.Unlock()
		}
	}
	if value, ok := inflight.Load(requestID); ok {
		request := value.(inflightRequest)
		request.Stage = name
		inflight.Store(requestID, request)
	}
}

func beginRequest(requestID string, request *http.Request) {
	if requestID == "" || request == nil {
		return
	}
	inflight.Store(requestID, inflightRequest{ID: requestID, Path: request.URL.Path, Method: request.Method, Started: time.Now().UTC(), Stage: "http_start"})
	stage(requestID, "http_start", map[string]any{"path": request.URL.Path, "method": request.Method})
}

func endRequest(requestID string, err error) {
	if requestID == "" {
		return
	}
	fields := map[string]any{}
	if err != nil {
		fields["error"] = sanitizeDiagnosticError(err.Error())
	}
	stage(requestID, "http_end", fields)
	inflight.Delete(requestID)
}

func sanitizeDiagnosticError(value string) string {
	// skipSpaceAfterMarker=false 保留原有观测行为：只吃掉 marker 之后紧跟的
	// 非分隔符片段，不跨越空白。
	return redactMarkedSecrets(value, diagnosticSecretMarkers, false)
}

// diagnosticSecretMarkers 是 stage 日志里需要打码的凭据前缀。
//
// 顺序即处理顺序：先打码具体的 token 前缀，再打码 "Authorization:" 这类整头
// 前缀。反过来的话 "Authorization: Bearer xxx" 会先被折成
// "Authorization:[redacted] Bearer xxx"，真正的 token 反而漏出去。
var diagnosticSecretMarkers = []string{"Bearer ", "access_token=", "accessToken=", "Authorization:"}

// redactMarkedSecrets 把每个 marker 后面的凭据值替换成 "[redacted]"，marker
// 本身原样保留。
//
// 实现要点是「只向前扫」：每次命中后游标推进到被打码值的末尾（end 严格大于
// 命中位置），下一轮从游标处继续搜索原串，绝不从 0 重新开始。之前的写法在原地
// 重写字符串后又从头 strings.Index，重新插入的 marker 会被反复命中 —— 字符串
// 里有两个及以上 marker 时就是死循环，一个 goroutine 吃满一个核。
//
// markers 按传入顺序逐个处理，每个 marker 独立做一次完整的前向扫描，因此每个
// marker 的每一次出现都会被打码。
//
// skipSpaceAfterMarker 为 true 时，marker 与凭据值之间的空格/制表符也一并计入
// 打码区间（形如 "Authorization: <token>" 的头部需要）。
func redactMarkedSecrets(value string, markers []string, skipSpaceAfterMarker bool) string {
	const replacement = "[redacted]"
	for _, marker := range markers {
		if marker == "" {
			continue
		}
		var builder strings.Builder
		cursor := 0
		for cursor < len(value) {
			offset := strings.Index(value[cursor:], marker)
			if offset < 0 {
				break
			}
			index := cursor + offset
			end := index + len(marker)
			if skipSpaceAfterMarker {
				for end < len(value) && (value[end] == ' ' || value[end] == '\t') {
					end++
				}
			}
			for end < len(value) && !strings.ContainsRune(" \t\n&\"',;", rune(value[end])) {
				end++
			}
			builder.WriteString(value[cursor:index])
			builder.WriteString(marker)
			builder.WriteString(replacement)
			// end > index >= cursor（marker 非空），游标严格前进，循环必然终止。
			cursor = end
		}
		if cursor > 0 {
			builder.WriteString(value[cursor:])
			value = builder.String()
		}
	}
	return value
}

func inflightSnapshot() []inflightRequest {
	out := make([]inflightRequest, 0)
	inflight.Range(func(_, value any) bool {
		if request, ok := value.(inflightRequest); ok {
			out = append(out, request)
		}
		return true
	})
	return out
}

func liveness() map[string]any {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	// 字段集对齐原版：status "alive"、chat 计数对象、numGC/sysBytes/
	// uptimeSeconds/stageLogPath；不使用上游的 chatSlots/inflight_count/now。
	return map[string]any{
		"status":         "alive",
		"chat":           chatSlotStats(),
		"inflight":       inflightSnapshot(),
		"heapAllocBytes": memory.HeapAlloc,
		"sysBytes":       memory.Sys,
		"numGC":          memory.NumGC,
		"goroutines":     runtime.NumGoroutine(),
		"uptimeSeconds":  int(time.Since(startedAt).Seconds()),
		"stageLogPath":   diagPath(),
	}
}

func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonOut(w, liveness())
}

func (s *Server) handleStageLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 原版返回 {lines, path}，日志未创建时附 note；
	// 不返回上游的 enabled/max_bytes/inflight/log。
	payload := map[string]any{
		"path":  diagPath(),
		"lines": []string{},
	}
	if data, err := os.ReadFile(diagPath()); err == nil {
		lines := []string{}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
		payload["lines"] = lines
	} else {
		payload["note"] = "stage log not created yet"
	}
	jsonOut(w, payload)
}

const (
	defaultMaxConcurrentChats = 64
	maxConfiguredChats        = 128
)

// maxConcurrentChats is the gateway-wide concurrency limit. It is deliberately
// higher than the historical APK default of four: the account pool and proxy
// pool can carry several independent WebSocket requests at the same time.
// M365_MAX_CONCURRENT_CHATS accepts 1..128 and defaults to sixty-four.
func maxConcurrentChats() int {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("M365_MAX_CONCURRENT_CHATS"))); err == nil && value >= 1 && value <= maxConfiguredChats {
		return value
	}
	return defaultMaxConcurrentChats
}

func currentChatSlots() chan struct{} {
	limit := maxConcurrentChats()
	chatSlotsMu.Lock()
	defer chatSlotsMu.Unlock()
	if chatSlots == nil || chatSlotN != limit {
		chatSlots = make(chan struct{}, limit)
		chatSlotN = limit
	}
	return chatSlots
}

func acquireChatSlot(ctx context.Context) (func(), error) {
	slots := currentChatSlots()
	select {
	case slots <- struct{}{}:
		chatSlotsMu.Lock()
		chatSlotTotal++
		if n := len(slots); n > chatSlotPeak {
			chatSlotPeak = n
		}
		chatSlotsMu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				select {
				case <-slots:
				default:
				}
			})
		}, nil
	case <-ctx.Done():
		chatSlotsMu.Lock()
		chatSlotRejected++
		chatSlotsMu.Unlock()
		return nil, ctx.Err()
	}
}

func releaseChatSlot() {
	slots := currentChatSlots()
	select {
	case <-slots:
	default:
	}
}

func chatSlotInflight() int {
	return len(currentChatSlots())
}

// chatSlotStats 复刻原版 /api/live 的 chat 对象。
func chatSlotStats() map[string]any {
	active := len(currentChatSlots())
	chatSlotsMu.Lock()
	defer chatSlotsMu.Unlock()
	return map[string]any{
		"active":   active,
		"limit":    chatSlotN,
		"peak":     chatSlotPeak,
		"rejected": chatSlotRejected,
		"total":    chatSlotTotal,
	}
}

func acquireChatSlotOrError(ctx context.Context) (func(), error) {
	release, err := acquireChatSlot(ctx)
	if err != nil {
		return nil, fmt.Errorf("global chat concurrency limit: %w", err)
	}
	return release, nil
}
