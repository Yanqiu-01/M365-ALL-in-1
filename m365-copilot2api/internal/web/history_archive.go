package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultHistoryCaptureLimit = 10
	maxHistoryCaptureLimit     = 100
	defaultHistoryMaxSnapshots = 100
)

// historyArchiveDocument is intentionally append-oriented. The live session
// cache keeps only the small, current context; this document keeps manual
// snapshots and the before/after pair for every detected compression event.
// Keeping both sides of a compression makes the archive lossless even when a
// client replaces old messages with a summary instead of retaining a strict
// suffix.
type historyArchiveDocument struct {
	SchemaVersion       int                      `json:"schemaVersion"`
	ConversationID      string                   `json:"conversationId"`
	SessionID           string                   `json:"sessionId,omitempty"`
	AccountID           string                   `json:"accountId,omitempty"`
	AccountEmail        string                   `json:"accountEmail,omitempty"`
	Title               string                   `json:"title,omitempty"`
	CreatedAt           time.Time                `json:"createdAt,omitempty"`
	UpdatedAt           time.Time                `json:"updatedAt,omitempty"`
	FirstCapturedAt     time.Time                `json:"firstCapturedAt,omitempty"`
	LastCapturedAt      time.Time                `json:"lastCapturedAt,omitempty"`
	CompressionCount    int                      `json:"compressionCount,omitempty"`
	CurrentContextBytes int64                    `json:"currentContextBytes,omitempty"`
	CurrentMessageCount int                      `json:"currentMessageCount,omitempty"`
	CurrentContext      []oaiMsg                 `json:"currentContext,omitempty"`
	Snapshots           []historyArchiveSnapshot `json:"snapshots,omitempty"`
	LastSnapshotHash    string                   `json:"lastSnapshotHash,omitempty"`
}

type historyArchiveSnapshot struct {
	CapturedAt         time.Time `json:"capturedAt"`
	Reason             string    `json:"reason"`
	Source             string    `json:"source,omitempty"`
	BeforeContextBytes int64     `json:"beforeContextBytes,omitempty"`
	AfterContextBytes  int64     `json:"afterContextBytes,omitempty"`
	BeforeMessageCount int       `json:"beforeMessageCount,omitempty"`
	AfterMessageCount  int       `json:"afterMessageCount,omitempty"`
	Messages           []oaiMsg  `json:"messages,omitempty"`
	BeforeContext      []oaiMsg  `json:"beforeContext,omitempty"`
	AfterContext       []oaiMsg  `json:"afterContext,omitempty"`
	DroppedMessages    []oaiMsg  `json:"droppedMessages,omitempty"`
	SnapshotHash       string    `json:"snapshotHash,omitempty"`
}

type historyArchiveStore struct {
	mu           sync.Mutex
	dir          string
	maxSnapshots int
}

func openHistoryArchive() *historyArchiveStore {
	maxSnapshots := boundedPositiveIntEnv("M365_HISTORY_MAX_SNAPSHOTS", defaultHistoryMaxSnapshots, 1, 1000)
	return &historyArchiveStore{
		dir:          historyDirectory(),
		maxSnapshots: maxSnapshots,
	}
}

// historyDirectory prefers the explicit setting. For a source checkout it
// also recognises the project layout so running from either the repository
// root, m365-copilot2api, or .build-out lands in the sibling History folder.
// A deployed binary should set M365_HISTORY_DIR explicitly when its working
// directory is outside the checkout.
func historyDirectory() string {
	if raw := strings.TrimSpace(os.Getenv("M365_HISTORY_DIR")); raw != "" {
		return filepath.Clean(raw)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "History"
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "History"
	}

	for dir := cwd; ; dir = filepath.Dir(dir) {
		base := strings.ToLower(filepath.Base(dir))
		switch base {
		case "m365-all-in-1":
			return filepath.Join(dir, "History")
		case "m365-copilot2api":
			return filepath.Join(filepath.Dir(dir), "History")
		case ".build-out":
			parent := filepath.Dir(dir)
			if strings.EqualFold(filepath.Base(parent), "m365-copilot2api") {
				return filepath.Join(filepath.Dir(parent), "History")
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return filepath.Join(cwd, "History")
}

func estimateContextBytes(messages []oaiMsg, hashes []string) int64 {
	if len(messages) == 0 {
		return 0
	}
	var total int64
	for i, message := range messages {
		if i < len(hashes) {
			total += int64(len(hashes[i]))
		} else {
			total += int64(len(message.Role) + len(contentToString(message.Content)))
		}
		// Account for JSON punctuation and fields that are not part of the
		// cached role/content hash. This is an estimate used only for detecting
		// a large shrink, not a wire-size contract.
		total += int64(len(message.Name) + len(message.ToolCallID) + len(message.ReasoningContent) + 32)
		for _, call := range message.ToolCalls {
			if b, err := json.Marshal(call); err == nil {
				total += int64(len(b))
			}
		}
	}
	return total
}

func historyFingerprint(messages []oaiMsg) string {
	b, err := json.Marshal(messages)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func messageFingerprint(message oaiMsg) string {
	b, err := json.Marshal(message)
	if err != nil {
		return fmt.Sprintf("%s\x00%s", message.Role, contentToString(message.Content))
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sameMessageSequence(a, b []oaiMsg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if messageFingerprint(a[i]) != messageFingerprint(b[i]) {
			return false
		}
	}
	return true
}

// messagesNotRetained returns the old messages that are absent from the new
// context. It treats equal messages as a multiset so repeated questions are
// not collapsed accidentally.
func messagesNotRetained(before, after []oaiMsg) []oaiMsg {
	counts := make(map[string]int, len(after))
	for _, message := range after {
		counts[messageFingerprint(message)]++
	}
	out := make([]oaiMsg, 0, len(before))
	for _, message := range before {
		key := messageFingerprint(message)
		if counts[key] > 0 {
			counts[key]--
			continue
		}
		out = append(out, message)
	}
	return out
}

func compressionEventFor(prev sessionBinding, next []oaiMsg, nextBytes int64, now time.Time, threshold int64, ratio float64) *compressionEvent {
	if threshold <= 0 {
		threshold = defaultCompressionThresholdBytes
	}
	if ratio <= 0 || ratio >= 1 {
		ratio = defaultCompressionRatio
	}
	if prev.ConversationID == "" || nextBytes <= 0 {
		return nil
	}
	if prev.ContextBytes <= 0 {
		prev.ContextBytes = estimateContextBytes(prev.ContextHistory, computeContentHashes(prev.ContextHistory))
	}
	if prev.ContextBytes < threshold || nextBytes >= prev.ContextBytes {
		return nil
	}
	if ratio <= 0 || ratio >= 1 || float64(nextBytes) > float64(prev.ContextBytes)*ratio {
		return nil
	}

	before := cloneMessages(prev.ContextHistory)
	after := cloneMessages(next)
	dropped := messagesNotRetained(before, after)
	// If the content changed in-place rather than being removed, retain the
	// complete old context as the conservative lossless fallback.
	if len(dropped) == 0 && !sameMessageSequence(before, after) {
		dropped = cloneMessages(before)
	}
	return &compressionEvent{
		SessionID:          prev.SessionID,
		ConversationID:     prev.ConversationID,
		AccountID:          prev.AccountID,
		CreatedAt:          prev.CreatedAt,
		DetectedAt:         now,
		BeforeContextBytes: prev.ContextBytes,
		AfterContextBytes:  nextBytes,
		BeforeMessages:     before,
		AfterMessages:      after,
		DroppedMessages:    dropped,
	}
}

func (a *historyArchiveStore) ensure() error {
	if a == nil {
		return errors.New("history archive is not initialized")
	}
	return os.MkdirAll(a.dir, 0o700)
}

func (a *historyArchiveStore) filename(conversationID string) string {
	clean := strings.TrimSpace(conversationID)
	if clean == "" {
		clean = "unknown"
	}
	var b strings.Builder
	for _, r := range clean {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if safe {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	base := strings.Trim(b.String(), ". ")
	if base == "" {
		base = "conversation"
	}
	if len(base) > 96 {
		base = base[:96]
	}
	// Always include a short digest. Besides preventing collisions after
	// sanitisation, this keeps the filename stable when IDs contain slashes.
	digest := sha256.Sum256([]byte(clean))
	return filepath.Join(a.dir, fmt.Sprintf("%s-%s.json", base, hex.EncodeToString(digest[:])[:12]))
}

func (a *historyArchiveStore) load(conversationID string) (historyArchiveDocument, string, error) {
	path := a.filename(conversationID)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return historyArchiveDocument{
			SchemaVersion:  1,
			ConversationID: conversationID,
			Snapshots:      []historyArchiveSnapshot{},
		}, path, nil
	}
	if err != nil {
		return historyArchiveDocument{}, path, err
	}
	var doc historyArchiveDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return historyArchiveDocument{}, path, fmt.Errorf("decode history archive %s: %w", path, err)
	}
	if doc.SchemaVersion == 0 {
		doc.SchemaVersion = 1
	}
	if doc.ConversationID == "" {
		doc.ConversationID = conversationID
	}
	if len(doc.Snapshots) > 0 {
		last := doc.Snapshots[len(doc.Snapshots)-1]
		doc.LastSnapshotHash = last.SnapshotHash
		if doc.LastSnapshotHash == "" {
			doc.LastSnapshotHash = historyFingerprint(last.Messages)
		}
	}
	return doc, path, nil
}

func (a *historyArchiveStore) save(path string, doc historyArchiveDocument) error {
	if err := a.ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func (a *historyArchiveStore) appendSnapshot(doc *historyArchiveDocument, snapshot historyArchiveSnapshot) bool {
	if snapshot.SnapshotHash != "" && snapshot.SnapshotHash == doc.LastSnapshotHash {
		return false
	}
	doc.Snapshots = append(doc.Snapshots, snapshot)
	doc.LastSnapshotHash = snapshot.SnapshotHash
	if a.maxSnapshots > 0 && len(doc.Snapshots) > a.maxSnapshots {
		doc.Snapshots = append([]historyArchiveSnapshot(nil), doc.Snapshots[len(doc.Snapshots)-a.maxSnapshots:]...)
	}
	return true
}

func updateArchiveIdentity(doc *historyArchiveDocument, conversationID, sessionID, accountID, accountEmail, title string, createdAt, updatedAt, now time.Time) {
	if doc.SchemaVersion == 0 {
		doc.SchemaVersion = 1
	}
	if doc.ConversationID == "" {
		doc.ConversationID = conversationID
	}
	if sessionID != "" {
		doc.SessionID = sessionID
	}
	if accountID != "" {
		doc.AccountID = accountID
	}
	if accountEmail != "" {
		doc.AccountEmail = accountEmail
	}
	if title != "" && title != "Untitled conversation" {
		doc.Title = title
	}
	if doc.CreatedAt.IsZero() || (!createdAt.IsZero() && createdAt.Before(doc.CreatedAt)) {
		doc.CreatedAt = createdAt
	}
	if !updatedAt.IsZero() && updatedAt.After(doc.UpdatedAt) {
		doc.UpdatedAt = updatedAt
	}
	if doc.FirstCapturedAt.IsZero() {
		doc.FirstCapturedAt = now
	}
	doc.LastCapturedAt = now
}

func updateArchiveMetadata(doc *historyArchiveDocument, conversationID, sessionID, accountID, accountEmail, title string, createdAt, updatedAt, now time.Time, current []oaiMsg, currentBytes int64) {
	updateArchiveIdentity(doc, conversationID, sessionID, accountID, accountEmail, title, createdAt, updatedAt, now)
	doc.CurrentContext = cloneMessages(current)
	doc.CurrentContextBytes = currentBytes
	doc.CurrentMessageCount = len(current)
}

func (a *historyArchiveStore) captureSession(sess sessionBinding, accountEmail string) (string, bool, error) {
	if a == nil {
		return "", false, errors.New("history archive is not initialized")
	}
	if strings.TrimSpace(sess.ConversationID) == "" {
		return "", false, errors.New("conversation id is empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().UTC()
	hashes := computeContentHashes(sess.ContextHistory)
	bytes := sess.ContextBytes
	if bytes <= 0 {
		bytes = estimateContextBytes(sess.ContextHistory, hashes)
	}
	doc, path, err := a.load(sess.ConversationID)
	if err != nil {
		return path, false, err
	}
	updateArchiveMetadata(&doc, sess.ConversationID, sess.SessionID, sess.AccountID, accountEmail, conversationTitle(sess.ContextHistory), sess.CreatedAt, sess.LastUsedAt, now, sess.ContextHistory, bytes)
	fingerprint := historyFingerprint(sess.ContextHistory)
	snapshot := historyArchiveSnapshot{
		CapturedAt:        now,
		Reason:            "manual",
		Source:            "gateway_context",
		AfterContextBytes: bytes,
		AfterMessageCount: len(sess.ContextHistory),
		Messages:          cloneMessages(sess.ContextHistory),
		SnapshotHash:      fingerprint,
	}
	added := a.appendSnapshot(&doc, snapshot)
	if !added {
		return path, false, nil
	}
	if err := a.save(path, doc); err != nil {
		return path, false, err
	}
	return path, true, nil
}

func (a *historyArchiveStore) recordCompression(event *compressionEvent, accountEmail string) (string, bool, error) {
	if a == nil {
		return "", false, errors.New("history archive is not initialized")
	}
	if event == nil || strings.TrimSpace(event.ConversationID) == "" {
		return "", false, errors.New("compression event is empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	doc, path, err := a.load(event.ConversationID)
	if err != nil {
		return path, false, err
	}
	updateArchiveMetadata(&doc, event.ConversationID, event.SessionID, event.AccountID, accountEmail, conversationTitle(event.AfterMessages), event.CreatedAt, event.DetectedAt, event.DetectedAt, event.AfterMessages, event.AfterContextBytes)
	fingerprint := historyFingerprint(event.BeforeMessages) + ":" + historyFingerprint(event.AfterMessages)
	snapshot := historyArchiveSnapshot{
		CapturedAt:         event.DetectedAt,
		Reason:             "compression",
		Source:             "compression_detector",
		BeforeContextBytes: event.BeforeContextBytes,
		AfterContextBytes:  event.AfterContextBytes,
		BeforeMessageCount: len(event.BeforeMessages),
		AfterMessageCount:  len(event.AfterMessages),
		BeforeContext:      cloneMessages(event.BeforeMessages),
		AfterContext:       cloneMessages(event.AfterMessages),
		DroppedMessages:    cloneMessages(event.DroppedMessages),
		SnapshotHash:       fingerprint,
	}
	if !a.appendSnapshot(&doc, snapshot) {
		return path, false, nil
	}
	doc.CompressionCount++
	if err := a.save(path, doc); err != nil {
		return path, false, err
	}
	return path, true, nil
}

func (a *historyArchiveStore) captureMetadata(conversationID, sessionID, accountID, accountEmail, title string, createdAt, updatedAt time.Time) (string, bool, error) {
	if a == nil {
		return "", false, errors.New("history archive is not initialized")
	}
	if strings.TrimSpace(conversationID) == "" {
		return "", false, errors.New("conversation id is empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().UTC()
	doc, path, err := a.load(conversationID)
	if err != nil {
		return path, false, err
	}
	updateArchiveIdentity(&doc, conversationID, sessionID, accountID, accountEmail, title, createdAt, updatedAt, now)
	fingerprint := fmt.Sprintf("metadata:%s:%s:%s", conversationID, updatedAt.UTC().Format(time.RFC3339Nano), title)
	snapshot := historyArchiveSnapshot{
		CapturedAt:        now,
		Reason:            "manual_metadata",
		Source:            "m365_cloud_metadata",
		AfterMessageCount: 0,
		SnapshotHash:      fingerprint,
	}
	if !a.appendSnapshot(&doc, snapshot) {
		return path, false, nil
	}
	if err := a.save(path, doc); err != nil {
		return path, false, err
	}
	return path, true, nil
}

type historyCaptureResult struct {
	Requested    int      `json:"requested"`
	Selected     int      `json:"selected"`
	Captured     int      `json:"captured"`
	MetadataOnly int      `json:"metadataOnly"`
	Skipped      int      `json:"skipped"`
	Files        []string `json:"files,omitempty"`
	Errors       []string `json:"errors,omitempty"`
	Directory    string   `json:"directory"`
}

// captureCandidates returns deep copies only for the selected sessions. The
// normal ListSessions path remains shallow to avoid adding allocations to
// every chat request.
func (sr *sessionResolver) captureCandidates(limit int) ([]sessionBinding, int) {
	if sr == nil || limit <= 0 {
		return nil, 0
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	all := make([]sessionBinding, 0, len(sr.sessions))
	for _, sess := range sr.sessions {
		all = append(all, sess)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].LastUsedAt.After(all[j].LastUsedAt)
	})
	seen := make(map[string]bool, len(all))
	selected := make([]sessionBinding, 0, limit)
	skipped := 0
	for _, sess := range all {
		if strings.TrimSpace(sess.ConversationID) == "" || seen[sess.ConversationID] {
			skipped++
			continue
		}
		seen[sess.ConversationID] = true
		if len(selected) >= limit {
			skipped++
			continue
		}
		sess.ContextHistory = cloneMessages(sess.ContextHistory)
		selected = append(selected, sess)
	}
	return selected, skipped
}

func (sr *sessionResolver) captureSessionsByConversation() map[string]sessionBinding {
	if sr == nil {
		return map[string]sessionBinding{}
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make(map[string]sessionBinding, len(sr.sessions))
	for _, sess := range sr.sessions {
		if strings.TrimSpace(sess.ConversationID) == "" {
			continue
		}
		prior, exists := out[sess.ConversationID]
		if exists && !sess.LastUsedAt.After(prior.LastUsedAt) {
			continue
		}
		sess.ContextHistory = cloneMessages(sess.ContextHistory)
		out[sess.ConversationID] = sess
	}
	return out
}
