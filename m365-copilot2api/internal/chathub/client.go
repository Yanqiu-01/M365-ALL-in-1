package chathub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ErrRateLimitNotice identifies the human-readable rate-limit response that
// ChatHub sometimes sends through the text channel instead of HTTP 429.
// Callers must independently probe the account before marking it unhealthy.
var ErrRateLimitNotice = errors.New("upstream rate-limit notice")

var ErrEmptyCompletion = errors.New("upstream returned empty completion; tone may be unavailable for this tenant")

const maxWebSocketSetupAttempts = 2

// DialError carries the HTTP status and optional Retry-After from a failed
// WebSocket dial so the web layer can route it into the correct cooldown.
type DialError struct {
	Status     int
	RetryAfter int
}

func (e *DialError) Error() string {
	return fmt.Sprintf("ws dial: upstream %d", e.Status)
}

var chTrace = os.Getenv("M365_TRACE") == "1"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const (
	rs          = "\x1e"
	defaultTone = "magic"
	wsBase      = "wss://substrate.office.com/m365Copilot/Chathub"
	// maxAttachments bounds per-request remote downloads: each image is
	// base64-encoded and held in memory alongside the multipart body.
	maxAttachments   = 10
	maxAttachmentMiB = 10
)

// Variants mirrored from the verified browser / Python probe.
const variants = "EnableMcpServerWidgets,feature.EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin,EnableRequestPlugins,feature.EnableSensitivityLabels,EnableUnsupportedUrlDetector,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3,feature.enablechatpages,feature.enableCodeCanvas,feature.turnOnWorkTabRecommendation,turnOffWorkTabUpsellFromClient,feature.turnOnDARecommendation,feature.IsStreamingModeInChatRequestEnabled,IncludeSourceAttributionsConcise,SkipPublishEmptyMessage,feature.EnableDeduplicatingSourceAttributions,Enable3PActionProgressMessages,feature.enableClientWebRtc,feature.EnableMeetingRecapOfSeriesMeetingWithCiq,feature.EnableReferencesListCompleteSignal,feature.StorageMessageSplitDisabled,feature.EnableCuaTakeControlApi,feature.cwcallowedos,feature.disabledisallowedmsgs,feature.enableCitationsForSynthesisData,feature.enableGenerateGraphicArtOptionsSet,cdximagen,feature.EnableUpdatedUXForConfirmationDialog,feature.EnableClientFileURLSupportForOfficeWebPaidCopilot,feature.EnableDesignEditorImageGrounding,feature.EnableDesignerEditor,feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix,feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix,feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix"

type Account struct {
	AccessToken string
	OID         string
	TID         string
}

type Request struct {
	Text           string
	Tone           string
	ConversationID string
	SessionID      string
	Attachments    []Attachment
	Tools          []Tool
	ToolChoice     any
	MCPServerURL   string // URL of the MCP HTTP SSE server for tool discovery
	// Started is true only for the first turn of a ChatHub conversation.
	Started bool
}

// StreamEvent is the protocol-neutral event exposed while ChatHub is still
// producing a response. Text events are safe to show immediately; progress and
// tool events are normally buffered by protocol adapters.
type StreamEvent struct {
	Kind        string
	Text        string
	MessageType string
	ContentType string
	ToolName    string
	Arguments   json.RawMessage
	Raw         json.RawMessage
}

type StreamHandler func(StreamEvent) error

type Result struct {
	Text           string
	Reasoning      string
	ConversationID string
	SessionID      string
	RequestID      string
	Throttling     any
	RawResult      string
	Events         []json.RawMessage
	Normalized     []Event
	Images         []string
}

type Client struct {
	HTTPHeader http.Header
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
	Trace      func(map[string]any)
}

func NewClient() *Client {
	identity := identityFor()
	h := make(http.Header)
	h.Set("Origin", "https://m365.cloud.microsoft")
	h.Set("User-Agent", identity.UserAgent)
	d := outbound.WebSocketDialer()
	return &Client{
		HTTPHeader: h,
		HTTPClient: outbound.HTTPClient(),
		Dialer:     d,
	}
}

// dialAndInitialize retries one alternate WebSocket setup path before any chat
// payload is sent. It intentionally stops before chatPayload / WriteMessage, so
// a retry cannot duplicate a user prompt or tool invocation.
func (c *Client) dialAndInitialize(ctx context.Context, wsURL string) (*websocket.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < maxWebSocketSetupAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, resp, err := c.Dialer.DialContext(ctx, wsURL, c.HTTPHeader.Clone())
		if err != nil {
			if resp != nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				retryAfter := 0
				if v, _ := strconv.Atoi(resp.Header.Get("Retry-After")); v > 0 {
					retryAfter = v
				}
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				log.Printf("chathub ws_dial %d Retry-After=%d", resp.StatusCode, retryAfter)
				return nil, &DialError{Status: resp.StatusCode, RetryAfter: retryAfter}
			}
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			lastErr = fmt.Errorf("ws dial: %w", err)
			if !shouldRetryWebSocketDial(ctx, attempt, resp) {
				return nil, lastErr
			}
			continue
		}

		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+rs)); err == nil {
			_, _, err = conn.ReadMessage()
		}
		if err == nil {
			return conn, nil
		}

		_ = conn.Close()
		lastErr = fmt.Errorf("upstream handshake failed: %w", err)
		if attempt+1 >= maxWebSocketSetupAttempts || ctx.Err() != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func shouldRetryWebSocketDial(ctx context.Context, attempt int, resp *http.Response) bool {
	if attempt+1 >= maxWebSocketSetupAttempts || ctx.Err() != nil {
		return false
	}
	// A confirmed account/rate-limit response is handled above. Other HTTP 5xx
	// responses and response-less dial errors can be caused by a single proxy
	// exit, so the second, payload-free setup attempt is safe and useful.
	return resp == nil || resp.StatusCode >= http.StatusInternalServerError
}

func (c *Client) Chat(ctx context.Context, acc Account, req Request) (Result, error) {
	return c.ChatWithDelta(ctx, acc, req, nil)
}

// ChatWithEvents is the compatibility entry point for the full event stream.
// The initial implementation exposes every upstream text delta immediately;
// the existing ChatWithDelta path remains the source of truth until the
// SignalR frame parser is migrated to emit progress/tool events as well.
func (c *Client) ChatWithEvents(ctx context.Context, acc Account, req Request, handler StreamHandler) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, func(text string) error {
		if handler == nil {
			return nil
		}
		return handler(StreamEvent{Kind: "text", Text: text})
	}, handler)
}

// ChatWithDelta preserves Chat semantics while exposing upstream text deltas as
// soon as SignalR delivers them. onDelta must return quickly; returning an error
// cancels the request. Full snapshot messages are retained for final-result
// reconstruction but are not emitted as deltas, preventing duplicate text.
func (c *Client) ChatWithDelta(ctx context.Context, acc Account, req Request, onDelta func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, onDelta, nil)
}

// ChatWithReasoning is the streaming entry point used by the OpenAI-compatible
// layer. onDelta receives answer text tokens, onReasoning receives the
// multi-step ChainOfThought transcript that ChatHub marks with
// contentOrigin=ChainOfThoughtSummary / addToChainOfThought=true.
func (c *Client) ChatWithReasoning(ctx context.Context, acc Account, req Request, onDelta func(string) error, onReasoning func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, onDelta, func(ev StreamEvent) error {
		if ev.Kind == "reasoning" && ev.Text != "" && onReasoning != nil {
			return onReasoning(ev.Text)
		}
		return nil
	})
}

func (c *Client) chatWithHandlers(ctx context.Context, acc Account, req Request, onDelta func(string) error, onEvent StreamHandler) (Result, error) {
	startedAt := time.Now()
	log.Printf("chathub timing start prompt_len=%d", len(req.Text))
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return Result{}, fmt.Errorf("missing access token / oid / tid")
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return Result{}, fmt.Errorf("empty prompt and no attachments")
	}
	if req.Tone == "" {
		req.Tone = defaultTone
	}
	firstTurn := req.Started
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
		firstTurn = true
	}
	if req.ConversationID == "" {
		req.ConversationID = uuid.NewString()
		firstTurn = true
	}
	requestID := uuid.NewString()
	wsURL, err := buildWSURL(acc, req.SessionID, req.ConversationID, requestID)
	if err != nil {
		return Result{}, err
	}
	attachCh := make(chan error, 1)
	if len(req.Attachments) > 0 {
		safeGoDeliver("uploadAttachments",
			func() { attachCh <- c.uploadAttachments(ctx, acc, req.ConversationID, req.Attachments) },
			func(err error) { attachCh <- err })
	}

	dialStarted := time.Now()
	// 每次请求都新建 WebSocket。wsURL 里带着本次请求的 chatsessionid /
	// clientrequestid / ConversationId，连接与会话是绑定的，跨请求复用会让
	// 上游把 payload 归到另一个会话上，因此原 APK 不做任何连接复用。
	conn, err := c.dialAndInitialize(ctx, wsURL)
	if err != nil {
		return Result{}, err
	}
	log.Printf("chathub timing ws_dial_ms=%d total_ms=%d", time.Since(dialStarted).Milliseconds(), time.Since(startedAt).Milliseconds())
	defer conn.Close()

	if len(req.Attachments) > 0 {
		if attachErr := <-attachCh; attachErr != nil {
			return Result{}, fmt.Errorf("upload attachment: %w", attachErr)
		}
	}

	payload := chatPayload(req.Text, req.SessionID, req.ConversationID, requestID, req.Tone, firstTurn, req.Attachments, req.Tools, req.ToolChoice, req.MCPServerURL)
	log.Printf("chathub prompt-trace text=%d tools=%d payload=%d", len(req.Text), len(req.Tools), len(payload))
	if c.Trace != nil {
		meta := map[string]any{"stage": "chathub_payload", "attachment_count": len(req.Attachments), "payload_has_attachments": strings.Contains(payload, `"attachments"`), "attachments": []map[string]any{}}
		for _, a := range req.Attachments {
			meta["attachments"] = append(meta["attachments"].([]map[string]any), map[string]any{"type": a.Type, "mime_type": a.MimeType, "url_length": len(a.URL), "data_url": strings.HasPrefix(a.URL, "data:"), "name": a.Name})
		}
		c.Trace(meta)
	}
	log.Printf("chathub timing handshake_ms=%d", time.Since(dialStarted).Milliseconds())
	payloadSentAt := time.Now()
	// APK wire_capture.go records the sanitized outbound chat payload before it
	// is written to the SignalR socket.
	recordWire("chat_send", wsURL, payload)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		return Result{}, fmt.Errorf("chat send: %w", err)
	}

	// 正文由 textStream 统一出口：它同时保存「已发给客户端的增量」和「上游最后
	// 一个完整快照」，因此上游挂附件/补引用时的非前缀重写不再丢内容，也不会
	// 造成复读。详见 snapshot_text.go。
	var stream *textStream
	stream = newTextStream(func(d string) error {
		delivered := stream.delivered()
		if chTrace {
			log.Printf("[trace:emitDelta] len=%d streamed=%d preview=%q", len(d), len(delivered)+len(d), truncate(d, 80))
		}
		if delivered == "" {
			log.Printf("chathub timing first_delta_ms=%d len=%d", time.Since(payloadSentAt).Milliseconds(), len(d))
		}
		if onDelta != nil {
			return onDelta(d)
		}
		return nil
	})
	emitDelta := stream.pushDelta
	emitUpdateText := stream.pushUpdateText
	var final string
	var throttling any
	var rawResult string
	var events []json.RawMessage
	seenStreamTools := map[string]bool{}
	var reasoningBuf strings.Builder
	// 思考内容改由 reasoningPump 逐帧即时推送，取材范围与完成帧兜底一致。
	reasoningPump := newReasoningPump(func(ev StreamEvent) error {
		if onEvent == nil {
			return nil
		}
		return onEvent(ev)
	})

	deadline := time.Now().Add(5 * time.Minute)
	type wsRead struct {
		msg []byte
		err error
	}
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		// ReadMessage 阻塞期间无法响应 ctx 取消，放入独立 goroutine 由 select 联动。
		readCh := make(chan wsRead, 1)
		safeGoDeliver("conn.ReadMessage",
			func() {
				_, msg, err := conn.ReadMessage()
				readCh <- wsRead{msg: msg, err: err}
			},
			func(err error) { readCh <- wsRead{err: err} })
		var read wsRead
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case read = <-readCh:
		}
		if read.err != nil {
			// Never convert a timeout or dropped WebSocket into a successful
			// partial response. A response is complete only after SignalR type 3.
			return Result{}, fmt.Errorf("ws read before completion: %w", read.err)
		}
		for _, part := range strings.Split(string(read.msg), rs) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if chTrace {
				log.Printf("[trace:ws] frame_len=%d preview=%q", len(part), truncate(part, 120))
			}
			events = append(events, json.RawMessage(append([]byte(nil), part...)))
			// 思考内容按帧即时推送。取材逻辑与完成帧的兜底完全一致，
			// 因此不再有「只能靠兜底补发」的差集 —— 这是思考时长被算成 0
			// 的根因。
			if err := reasoningPump.push(events[len(events)-1]); err != nil {
				return Result{}, err
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(part), &obj); err != nil {
				continue
			}
			t, _ := obj["type"].(float64)
			target, _ := obj["target"].(string)

			// SignalR ping
			if int(t) == 6 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
				continue
			}

			if int(t) == 1 && target == "update" {
				args, _ := obj["arguments"].([]any)
				for _, raw := range args {
					arg, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					msgs, _ := arg["messages"].([]any)
					if err := emitUpdateEvents(arg, msgs, seenStreamTools, onEvent); err != nil {
						return Result{}, err
					}
					if thr, ok := arg["throttling"]; ok {
						throttling = thr
					}
					// 正文按上游声明的语义分派：streamingMode=Delta 的
					// writeAtCursor 是增量片段，必须累积拼接；其余是完整快照。
					// 此前一律按快照处理，增量之间互相替换，只有最后一段能
					// 留下来（实测 `hello` + `.py` + `](url)` 只剩 `](url)`）。
					for _, item := range updateTexts(arg, msgs) {
						if err := emitUpdateText(item); err != nil {
							return Result{}, err
						}
					}
				}
				continue
			}

			if int(t) == 2 {
				item, _ := obj["item"].(map[string]any)
				if item != nil {
					if thr, ok := item["throttling"]; ok {
						throttling = thr
					}
					if res, ok := item["result"].(map[string]any); ok {
						rawResult, _ = res["value"].(string)
						if msg, ok := res["message"].(string); ok {
							final = msg
						}
					}
				}
				// completion frame often follows; keep reading a bit but we already have content
				continue
			}

			if int(t) == 3 {
				if errObj, ok := obj["error"].(map[string]any); ok {
					return Result{}, fmt.Errorf("chathub completion error: %v", errObj)
				}
				log.Printf("chathub timing completion_frame_ms=%d streamed_text=%d events=%d", time.Since(payloadSentAt).Milliseconds(), len(stream.delivered()), len(events))
				// 补发上游快照里尚未送达客户端的部分（含被抑制的抖动窗口），
				// 只追加不会造成复读的内容。
				if err := stream.finalize(); err != nil {
					return Result{}, err
				}
				if stream.delivered() == "" && final != "" {
					if err := emitDelta(final); err != nil {
						return Result{}, err
					}
				}
				// 最终文本以上游最后一个完整快照为真相：中途的非前缀重写
				// （挂附件、补引用）不能让返回内容残缺。
				stream.logReconciliation()
				text := stream.finalText()
				// 兜底取材同样要经过引用标记剥离，否则非流式路径会漏出私用区
				// 字符，与流式出口不一致。剥离规则见 citation_markers.go。
				if text == "" {
					text = stripCiteMarkers(final)
				}
				if text == "" {
					text = stripCiteMarkers(stream.joinedDeltas())
				}
				// 原 APK 不对回复文本做限流/拒绝判定，也没有
				// ErrEmptyCompletion：模型的短回答（含「很抱歉，我无法
				// 响应」这类内容层拒绝）是成功响应，必须原样返回。
				// 传输层异常另由 classifyUpstream 一侧的 ws dial /
				// ws read before completion / completion error 覆盖。
				// pump 已按帧推送并累计全部思考内容，直接采用即可；
				// 不再从原始帧重算，避免与已发送的增量重复。
				pumpedReasoning := reasoningPump.text()
				reasoning := pumpedReasoning
				if reasoning == "" {
					reasoning = reasoningBuf.String()
				}
				// 极端兜底：pump 与实时累积都为空时才回退到全量扫描。
				// 此时必须把兜底内容也作为一个 reasoning 事件发出去；
				// 只填 Result.Reasoning 会让非流式有思考内容，而流式客户端
				// 看不到任何 reasoning_content。
				if reasoning == "" {
					reasoning = reasoningFromFrames(events)
				}
				if onEvent != nil && reasoning != "" {
					fallback := reasoning
					if pumpedReasoning != "" {
						if strings.HasPrefix(reasoning, pumpedReasoning) {
							fallback = reasoning[len(pumpedReasoning):]
						} else {
							fallback = ""
						}
					}
					if fallback != "" {
						if err := onEvent(StreamEvent{Kind: "reasoning", Text: fallback}); err != nil {
							return Result{}, err
						}
					}
				}
				return Result{
					Text:           text,
					Reasoning:      reasoning,
					ConversationID: req.ConversationID,
					SessionID:      req.SessionID,
					RequestID:      requestID,
					Throttling:     throttling,
					RawResult:      rawResult,
					Events:         events,
					Normalized:     NormalizeEvents(events),
					Images:         imageURLs(events),
				}, nil
			}
		}
	}

	// Reaching the overall deadline without a SignalR completion frame is
	// an incomplete upstream response. Do not return accumulated deltas as if
	// they were a successful, finished answer.
	return Result{}, fmt.Errorf("chathub response deadline exceeded before completion")
}

func buildWSURL(acc Account, sessionID, conversationID, requestID string) (string, error) {
	identity := identityFor()
	q := url.Values{}
	q.Set("chatsessionid", requestID)
	q.Set("clientrequestid", requestID)
	q.Set("X-SessionId", sessionID)
	q.Set("ConversationId", conversationID)
	q.Set("access_token", acc.AccessToken)
	q.Set("variants", variants)
	// source must keep quotes like the browser probe.
	q.Set("source", fmt.Sprintf(`"%s"`, identity.Source))
	q.Set("product", identity.ProductThread)
	q.Set("agentHost", "Bizchat.FullScreen")
	q.Set("licenseType", "Starter")
	q.Set("agent", "web")
	q.Set("scenario", "OfficeWebIncludedCopilot")

	// url.Values encodes quotes; probe used safe='",' so keep quotes unescaped-ish.
	// Gorilla/url will encode " to %22 which MS accepts.
	u := fmt.Sprintf("%s/%s@%s?%s", wsBase, acc.OID, acc.TID, q.Encode())
	return u, nil
}

func (c *Client) uploadAttachments(ctx context.Context, acc Account, conversationID string, attachments []Attachment) error {
	imageCount := 0
	for i := range attachments {
		a := &attachments[i]
		if a.Type != "image" {
			continue
		}
		imageCount++
		if imageCount > maxAttachments {
			return fmt.Errorf("too many image attachments: limit is %d", maxAttachments)
		}
		// For non-data URLs, download the image first
		imageData := a.URL
		if !strings.HasPrefix(a.URL, "data:") {
			if err := validateRemoteDownloadURL(a.URL); err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
			if err != nil {
				continue
			}
			resp, err := c.HTTPClient.Do(req)
			if err != nil {
				continue
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentMiB<<20))
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				continue
			}
			mimeType := resp.Header.Get("Content-Type")
			if mimeType == "" {
				mimeType = "image/png"
			}
			imageData = "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body)
		}
		comma := strings.IndexByte(imageData, ',')
		if comma < 0 {
			return fmt.Errorf("invalid image data URL")
		}
		encoded := imageData[comma+1:]
		if strings.Contains(strings.ToLower(imageData[:comma]), ";base64") == false {
			return fmt.Errorf("image URL is not base64")
		}
		if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
			return fmt.Errorf("decode image: %w", err)
		}
		form := url.Values{}
		form.Set("scenario", "UploadImage")
		form.Set("conversationId", conversationID)
		// The browser sends the complete data URL in FileBase64, including the
		// media-type prefix. UploadFile accepts this form and returns docId.
		// Live-verified 2026-08-08: UploadFile rejects multipart bodies
		// (HTTP 400 InvalidRequest); it requires x-www-form-urlencoded like
		// PyRIT's httpx client sends.
		form.Set("FileBase64", imageData)
		if c.Trace != nil {
			c.Trace(map[string]any{"stage": "upload_start", "index": i, "conversation_id": conversationID, "mime_type": a.MimeType, "base64_length": len(encoded), "token_present": acc.AccessToken != ""})
		}
		form.Add("optionsSets", "cwcgptvsan")
		form.Add("optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://substrate.office.com/m365Copilot/UploadFile", strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if acc.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		}
		req.Header.Set("Accept", "application/json")
		// Required by the enterprise Copilot UploadFile image-input path.
		// This feature gate is documented in the prior reverse-proxy research
		// and mirrors the PyRIT request flow.
		req.Header.Set("X-Variants", "feature.EnableImageSupportInUploadFile")
		req.Header.Set("X-Scenario", "OfficeWebIncludedCopilot")
		req.Header.Set("Referer", "https://m365.cloud.microsoft/")
		for k, vv := range c.HTTPHeader {
			for _, v := range vv {
				if k != "Origin" || v != "" {
					req.Header.Add(k, v)
				}
			}
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			log.Printf("[upload] http error: %v", err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			log.Printf("[upload] read error: %v", readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("[upload] status %s: %s", resp.Status, strings.TrimSpace(string(data[:minInt(len(data), 500)])))
			continue
		}
		var out struct {
			DocID    string `json:"docId"`
			FileName string `json:"fileName"`
			FileType string `json:"fileType"`
			Result   struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			log.Printf("[upload] json error: %v", err)
			continue
		}
		if out.Result.Value != "Success" || out.DocID == "" {
			log.Printf("[upload] failed: %s", strings.TrimSpace(string(data)))
			continue
		}
		a.DocID = out.DocID
		a.FileType = strings.TrimPrefix(strings.ToLower(out.FileType), ".")
		// ChatHub's ImageFile annotation uses jpg for JPEG uploads.
		if a.FileType == "jpeg" {
			a.FileType = "jpg"
		}
		if a.Name == "" {
			a.Name = out.FileName
		}
		if c.Trace != nil {
			c.Trace(map[string]any{"stage": "upload_success", "doc_id": a.DocID, "file_name": a.Name, "file_type": a.FileType})
		}
	}
	return nil
}

func chatPayload(text, sessionID, conversationID, requestID, tone string, firstTurn bool, attachments []Attachment, tools []Tool, toolChoice any, mcpServerURL string) string {
	identity := identityFor()
	text = toolProtocolPrompt(text, tools, toolChoice, len(clientPlugins(tools, mcpServerURL)) > 0)
	message := map[string]any{
		"author":                "user",
		"attachments":           attachments,
		"inputMethod":           "Keyboard",
		"text":                  text,
		"entityAnnotationTypes": []string{"People", "File", "Event", "Email", "TeamsMessage"},
		"requestId":             requestID,
		"locationInfo": map[string]any{
			"timeZoneOffset": 8,
			"timeZone":       "Asia/Shanghai",
		},
		"locale":            "zh-cn",
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}
	// The browser does not send an OpenAI attachments array to ChatHub. It
	// sends a file annotation after the file has been uploaded by Office.
	annotations := make([]any, 0, len(attachments))
	for _, a := range attachments {
		if a.Type != "image" || a.DocID == "" {
			continue
		}
		if a.Name == "" {
			a.Name = "image." + a.FileType
		}
		fileType := a.FileType
		if fileType == "" {
			fileType = strings.TrimPrefix(strings.ToLower(a.MimeType), "image/")
		}
		if fileType == "" || fileType == "image" || fileType == "*" {
			fileType = "jpg"
		}
		annotations = append(annotations, map[string]any{
			"id": a.DocID,
			"messageAnnotationMetadata": map[string]any{
				"@type": "File", "annotationType": "File",
				"fileType": fileType, "fileName": a.Name,
			},
			"messageAnnotationType": "ImageFile",
		})
	}
	if len(annotations) > 0 {
		message["messageAnnotations"] = annotations
		message["connectedFederatedConnections"] = []string{"dummyId"}
	}
	// Restore the old gateway's multimodal injection path. The historical
	// implementation merged imageUrl/imageBase64 directly into message rather
	// than relying solely on the newer attachments array.
	for _, a := range attachments {
		if a.Type != "image" || a.URL == "" {
			continue
		}
		if strings.HasPrefix(a.URL, "data:") {
			if comma := strings.IndexByte(a.URL, ','); comma >= 0 && comma+1 < len(a.URL) {
				message["imageBase64"] = a.URL[comma+1:]
			}
		} else {
			message["imageUrl"] = a.URL
		}
		break
	}
	optionsSets := []any{
		"search_result_progress_messages_with_search_queries",
		"update_textdoc_response_after_streaming",
		"deepleo_networking_timeout_10minutes_canmore",
		"cwc_flux_image",
		"cwcfluxgptv",
		"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
		"gptvnorm2048",
		"cwc_fileupload_odb",
		"update_memory_plugin",
		"add_custom_instructions",
		"cwc_flux_v3",
		"flux_v3_progress_messages",
		"enable_batch_token_processing",
		"enable_gg_gpt",
	}
	for _, option := range identity.ExtraOptions {
		optionsSets = append(optionsSets, option)
	}
	chat := map[string]any{
		"arguments": []any{
			map[string]any{
				"source":              identity.Source,
				"clientCorrelationId": uuid.NewString(),
				"sessionId":           sessionID,
				"optionsSets":         optionsSets,
				"options":             map[string]any{},
				"allowedMessageTypes": []string{
					"Chat", "Suggestion", "Disengaged", "Progress", "EndOfRequest", "InternalLoaderMessage",
				},
				"sliceIds":          []any{},
				"threadLevelGptId":  map[string]any{},
				"conversationId":    conversationID,
				"traceId":           uuid.NewString(),
				"isStartOfSession":  firstTurn,
				"productThreadType": identity.ProductThread,
				"clientInfo": map[string]any{
					"clientPlatform": identity.ClientPlatform,
					"clientAppName":  identity.ClientAppName,
				},
				"tone":          tone,
				"streamingMode": "ConciseWithPadding",
				"message":       message,

				"plugins":    clientPlugins(tools, mcpServerURL),
				"toolChoice": toolChoice,
			},
		},
		"invocationId": "0",
		"target":       "chat",
		"type":         4,
	}
	metrics := map[string]any{
		"arguments": []any{
			map[string]any{
				"Timestamps": map[string]string{
					"ConnectionStart":       "",
					"UserInputStart":        "",
					"ConnectionEstablished": "",
					"UserInputSubmit":       "",
				},
			},
		},
		"target": "Metrics",
		"type":   1,
	}
	b1, _ := json.Marshal(chat)
	b2, _ := json.Marshal(metrics)
	return string(b1) + rs + string(b2) + rs
}
