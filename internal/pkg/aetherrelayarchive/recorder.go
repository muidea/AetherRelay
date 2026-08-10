package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Recorder struct {
	root          string
	maxRounds     int
	fullContent   bool
	scopeByAPIKey bool
	mu            sync.Mutex
	next          int
	active        map[int]struct{}
}

type Round struct {
	ID                 int       `json:"id"`
	Dir                string    `json:"dir"`
	StartedAt          time.Time `json:"started_at"`
	RequestID          string    `json:"request_id,omitempty"`
	APIKeyID           string    `json:"api_key_id,omitempty"`
	StablePrefixHash   string    `json:"stable_prefix_hash,omitempty"`
	RequestFingerprint string    `json:"request_fingerprint,omitempty"`
	// Transport plan fields (in-memory only; written into Metadata at finish).
	Operation          string
	ClientEndpoint     string
	ClientProtocol     string
	UpstreamProtocol   string
	UpstreamEndpoint   string
	ConversionMode     string
	ConversionLevel    int
	ConversionDuration time.Duration
	ConversionDegraded bool
	// IgnoredFeatures records explicitly compatibility-degraded request fields.
	// It is populated by a bounded protocol adapter and never contains payload
	// values, so metadata can explain a degraded request without retaining
	// sensitive input.
	IgnoredFeatures     []string
	UnsupportedFeatures []string
	// UpstreamDuration 是本次上游 HTTP 请求（含首包探测）的耗时，仅供
	// usage 结算使用；完整 metadata 当前仍保留总请求耗时。
	UpstreamDuration         time.Duration
	UpstreamStatus           int
	UpstreamContentType      string
	UpstreamContentLength    int64
	UpstreamTransferEncoding string
	recorder                 *Recorder
	written                  map[string]struct{} // basename -> present
}

func (r *Round) markWritten(name string) {
	if r == nil || name == "" {
		return
	}
	if r.written == nil {
		r.written = map[string]struct{}{}
	}
	r.written[name] = struct{}{}
}

// HasFile 报告本 round 是否成功写入过指定 basename。
func (r *Round) HasFile(name string) bool {
	if r == nil || r.written == nil {
		return false
	}
	_, ok := r.written[name]
	return ok
}

func (r *Round) SetRequestID(id string) {
	if r == nil {
		return
	}
	r.RequestID = id
}

func (r *Round) SetAPIKeyID(id string) {
	if r == nil {
		return
	}
	r.APIKeyID = id
}

func (r *Round) SetFingerprint(stableHash, fingerprint string) {
	if r == nil {
		return
	}
	r.StablePrefixHash = stableHash
	r.RequestFingerprint = fingerprint
}

// SetTransportPlan 记录本次请求的 TransportPlan 权威字段,供 Metadata 统一落盘。
func (r *Round) SetTransportPlan(operation, clientEndpoint, clientProtocol, upstreamProtocol, upstreamEndpoint, conversionMode string) {
	if r == nil {
		return
	}
	r.Operation = operation
	r.ClientEndpoint = clientEndpoint
	r.ClientProtocol = clientProtocol
	r.UpstreamProtocol = upstreamProtocol
	r.UpstreamEndpoint = upstreamEndpoint
	r.ConversionMode = conversionMode
	r.ConversionDegraded = false
	if conversionMode == "responses_to_anthropic" || conversionMode == "anthropic_to_responses" {
		r.ConversionLevel = 1
	} else if conversionMode == "openai_to_anthropic" || conversionMode == "anthropic_to_openai" {
		r.ConversionLevel = 1
	} else {
		r.ConversionLevel = 0
	}
}

// SetConversionLevel records the validated model/direction capability used by
// this round. Native paths remain level 0; invalid values are clamped away so
// archive metadata cannot publish an unsupported level.
func (r *Round) SetConversionLevel(level int) {
	if r == nil {
		return
	}
	if level < 0 || level > 3 {
		level = 0
	}
	r.ConversionLevel = level
}

func (r *Round) SetUpstreamDuration(duration time.Duration) {
	if r == nil {
		return
	}
	r.UpstreamDuration = duration
}

func (r *Round) SetUpstreamHeaders(status int, contentType string, contentLength int64, transferEncoding string, headerDuration time.Duration) {
	if r == nil {
		return
	}
	r.UpstreamStatus = status
	r.UpstreamContentType = contentType
	r.UpstreamContentLength = contentLength
	r.UpstreamTransferEncoding = transferEncoding
	r.UpstreamDuration = headerDuration
}

// SetIgnoredFeatures records the request fields intentionally not represented
// by a bounded adapter. Callers must pass field names only, never values.
func (r *Round) SetIgnoredFeatures(features []string) {
	if r == nil {
		return
	}
	r.IgnoredFeatures = append([]string(nil), features...)
}

type Metadata struct {
	ID         int       `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	RequestID  string    `json:"request_id,omitempty"`
	// EventID 与 usage_events.event_id 对齐(通常等于 request_id)。
	EventID string `json:"event_id,omitempty"`
	// APIKeyID 为客户端身份稳定 ID;永不保存原始密钥。
	APIKeyID string `json:"api_key_id,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Operation / ClientEndpoint / Upstream* / ConversionMode 记录 TransportPlan 权威字段。
	Operation              string   `json:"operation,omitempty"`
	ClientEndpoint         string   `json:"client_endpoint,omitempty"`
	ClientProtocol         string   `json:"client_protocol,omitempty"`
	UpstreamProtocol       string   `json:"upstream_protocol,omitempty"`
	UpstreamEndpoint       string   `json:"upstream_endpoint,omitempty"`
	ConversionMode         string   `json:"conversion_mode,omitempty"`
	ConversionLevel        int      `json:"conversion_level,omitempty"`
	IgnoredFeatures        []string `json:"ignored_features,omitempty"`
	UnsupportedFeatures    []string `json:"unsupported_features,omitempty"`
	ConversionDurationMS   int64    `json:"conversion_duration_ms,omitempty"`
	ConversionDegraded     bool     `json:"conversion_degraded,omitempty"`
	StablePrefixHash       string   `json:"stable_prefix_hash,omitempty"`
	RequestFingerprint     string   `json:"request_fingerprint,omitempty"`
	StablePrefixDrift      bool     `json:"stable_prefix_drift,omitempty"`
	StablePrefixDriftCount int      `json:"stable_prefix_drift_count,omitempty"`
	Stream                 bool     `json:"stream"`
	HTTPStatus             int      `json:"http_status"`
	// Outcome 与 DuckDB/Prometheus 对齐的业务结果枚举。
	Outcome                  string  `json:"outcome,omitempty"`
	DurationMS               int64   `json:"duration_ms"`
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	TotalTokens              int     `json:"total_tokens"`
	CachedInputTokens        int     `json:"cached_input_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheHitRate             float64 `json:"cache_hit_rate"`
	Estimated                bool    `json:"estimated"`
	RequestPath              string  `json:"request_path,omitempty"`
	RequestMetaPath          string  `json:"request_meta_path,omitempty"`
	UpstreamRequestPath      string  `json:"upstream_request_path,omitempty"`
	UpstreamResponsePath     string  `json:"upstream_response_path,omitempty"`
	ResponsePath             string  `json:"response_path,omitempty"`
	FullResponsePath         string  `json:"full_response_path,omitempty"`
	// FullContentEnabled 标明配置是否启用完整正文归档(不保证磁盘写入一定成功)。
	FullContentEnabled bool   `json:"full_content_enabled"`
	Error              string `json:"error,omitempty"`
}

func (r *Round) SetUnsupportedFeatures(features []string) {
	if r == nil {
		return
	}
	r.UnsupportedFeatures = append([]string(nil), features...)
}

func (r *Round) SetConversionDuration(duration time.Duration) {
	if r == nil {
		return
	}
	r.ConversionDuration = duration
}

func (r *Round) SetConversionDegraded(degraded bool) {
	if r == nil {
		return
	}
	r.ConversionDegraded = degraded
}

func NewRecorder(root string, maxRounds ...int) (*Recorder, error) {
	return NewRecorderOptions(root, RecorderOptions{MaxRounds: firstInt(maxRounds, 500), FullContent: true})
}

// RecorderOptions 控制归档行为。
type RecorderOptions struct {
	MaxRounds   int
	FullContent bool
	// ScopeByAPIKey partitions rounds by the authenticated client key ID.
	ScopeByAPIKey bool
}

// NewRecorderOptions 使用显式选项构造归档器。
func NewRecorderOptions(root string, opts RecorderOptions) (*Recorder, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	next, err := nextSequence(root)
	if err != nil {
		return nil, err
	}
	max := opts.MaxRounds
	if max <= 0 {
		max = 500
	}
	recorder := &Recorder{root: root, maxRounds: max, fullContent: opts.FullContent, next: next, active: map[int]struct{}{}, scopeByAPIKey: opts.ScopeByAPIKey}
	if opts.ScopeByAPIKey {
		if err := recorder.recoverIncompleteLocked(); err != nil {
			return nil, err
		}
	}
	if err := recorder.cleanupLocked(); err != nil {
		return nil, err
	}
	return recorder, nil
}

// FullContent 报告是否落盘完整请求/响应正文。
func (r *Recorder) FullContent() bool {
	if r == nil {
		return false
	}
	return r.fullContent
}

// FullContent 报告本 round 所属归档器是否落盘完整正文。
func (r *Round) FullContent() bool {
	if r == nil || r.recorder == nil {
		return true // nil recorder 视为不限制(测试场景)
	}
	return r.recorder.fullContent
}

func firstInt(values []int, fallback int) int {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

func (r *Recorder) Start() (*Round, error) {
	return r.start("", true)
}

// StartForAPIKey starts an archive round in a dedicated API-key scope.
// The key ID is an internal stable identifier, never the raw secret.
func (r *Recorder) StartForAPIKey(apiKeyID string) (*Round, error) {
	if !r.scopeByAPIKey {
		return r.Start()
	}
	return r.start(apiKeyID, false)
}

func (r *Recorder) start(apiKeyID string, legacy bool) (*Round, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	id := r.next
	r.next++
	r.active[id] = struct{}{}
	r.mu.Unlock()

	scope := archiveScope(apiKeyID)
	dir := filepath.Join(r.root, scope, fmt.Sprintf("%06d", id))
	if legacy {
		scope = ""
		dir = filepath.Join(r.root, fmt.Sprintf("%06d", id))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
		return nil, err
	}
	return &Round{ID: id, Dir: dir, APIKeyID: apiKeyID, StartedAt: time.Now(), recorder: r, written: map[string]struct{}{}}, nil
}

func archiveScope(apiKeyID string) string {
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return "anonymous"
	}
	original := apiKeyID
	var b strings.Builder
	for _, ch := range apiKeyID {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "anonymous"
	}
	scope := b.String()
	if scope != original {
		sum := sha256.Sum256([]byte(original))
		scope += "-" + hex.EncodeToString(sum[:4])
	}
	return scope
}

// RemoveAPIKeyScope removes all interaction archives for one client API key.
// The caller should revoke the key before invoking this function.
func RemoveAPIKeyScope(root, apiKeyID string) error {
	root = strings.TrimSpace(root)
	if root == "" || strings.TrimSpace(apiKeyID) == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(root, archiveScope(apiKeyID)))
}

func (r *Round) WriteRequest(body []byte) error {
	if r == nil {
		return nil
	}
	if r.recorder != nil && !r.recorder.fullContent {
		return nil
	}
	if err := writeJSONOrRaw(filepath.Join(r.Dir, "request.json"), body); err != nil {
		return err
	}
	r.markWritten("request.json")
	return nil
}

func (r *Round) WriteJSON(name string, value any) error {
	if r == nil {
		return nil
	}
	if name == "" {
		name = "metadata.json"
	}
	encoded, err := marshalJSON(value)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(r.Dir, name), encoded, 0o600); err != nil {
		return err
	}
	r.markWritten(name)
	return nil
}

func (r *Round) WriteResponse(name string, body []byte) error {
	if r == nil {
		return nil
	}
	if r.recorder != nil && !r.recorder.fullContent {
		return nil
	}
	if name == "" {
		name = "response.bin"
	}
	if err := writeFileAtomic(filepath.Join(r.Dir, name), body, 0o600); err != nil {
		return err
	}
	r.markWritten(name)
	return nil
}

func (r *Round) CreateResponseWriter(name string) (io.WriteCloser, error) {
	if r == nil {
		return nil, nil
	}
	if r.recorder != nil && !r.recorder.fullContent {
		return nopWriteCloser{}, nil
	}
	if name == "" {
		name = "response.bin"
	}
	f, err := os.OpenFile(filepath.Join(r.Dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	r.markWritten(name)
	return f, nil
}

// WriteMetadata 写入 metadata.json 并释放 active 状态。
// 无论 metadata 写入是否成功,都会调用 finish 释放 active,避免 I/O 失败后
// round 永久占用 active map、阻塞 retention 清理。
// 返回值优先保留 metadata 写入错误;finish/cleanup 错误在 metadata 成功时返回。
func (r *Round) WriteMetadata(metadata Metadata) error {
	if r == nil {
		return nil
	}
	metadata.ID = r.ID
	metadata.StartedAt = r.StartedAt
	if metadata.RequestID == "" {
		metadata.RequestID = r.RequestID
	}
	if metadata.EventID == "" {
		metadata.EventID = r.RequestID
	}
	if metadata.APIKeyID == "" {
		metadata.APIKeyID = r.APIKeyID
	}
	if metadata.StablePrefixHash == "" {
		metadata.StablePrefixHash = r.StablePrefixHash
	}
	if metadata.RequestFingerprint == "" {
		metadata.RequestFingerprint = r.RequestFingerprint
	}
	if metadata.FinishedAt.IsZero() {
		metadata.FinishedAt = time.Now()
	}
	writeErr := r.WriteJSON("metadata.json", metadata)
	// 无论写入成败都释放 active;目录可留给运维排查,但不得永久跳过 retention。
	finishErr := r.finish()
	if writeErr != nil {
		return writeErr
	}
	return finishErr
}

// Abort 在请求中途放弃时释放 active 状态(不写 metadata)。
// 幂等:重复调用安全。用于 handler 在无法完成 WriteMetadata 时保证生命周期闭合。
func (r *Round) Abort() error {
	if r == nil {
		return nil
	}
	return r.finish()
}

func (r *Round) finish() error {
	if r == nil || r.recorder == nil {
		return nil
	}
	return r.recorder.finish(r.ID)
}

func (r *Recorder) finish(id int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[id]; !ok {
		// 已释放:跳过重复 retention 扫描(正常路径 WriteMetadata + defer Abort 会二次调用)。
		return nil
	}
	delete(r.active, id)
	return r.cleanupLocked()
}

func (r *Recorder) cleanupLocked() error {
	if r == nil || r.maxRounds <= 0 {
		return nil
	}
	dirs, err := listScopedNumericDirs(r.root)
	if err != nil {
		return err
	}
	byScope := map[string][]archiveDir{}
	for _, dir := range dirs {
		if _, ok := r.active[dir.id]; ok {
			continue
		}
		byScope[dir.scope] = append(byScope[dir.scope], dir)
	}
	for _, scoped := range byScope {
		if len(scoped) <= r.maxRounds {
			continue
		}
		sort.Slice(scoped, func(i, j int) bool { return scoped[i].id < scoped[j].id })
		for _, dir := range scoped[:len(scoped)-r.maxRounds] {
			path := filepath.Join(r.root, dir.name)
			if dir.scope != "" {
				path = filepath.Join(r.root, dir.scope, dir.name)
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Recorder) recoverIncompleteLocked() error {
	dirs, err := listScopedNumericDirs(r.root)
	if err != nil {
		return err
	}
	for _, item := range dirs {
		if item.scope == "" {
			continue
		}
		dir := filepath.Join(r.root, item.scope, item.name)
		metadataPath := filepath.Join(dir, "metadata.json")
		if _, err := os.Stat(metadataPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		meta := Metadata{
			ID:                 item.id,
			StartedAt:          info.ModTime(),
			FinishedAt:         time.Now(),
			APIKeyID:           item.scope,
			HTTPStatus:         500,
			Outcome:            "process_interrupted",
			FullContentEnabled: r.fullContent,
			Error:              "service restarted before interaction archive completed",
		}
		paths := []struct {
			name   string
			target *string
		}{
			{"request.json", &meta.RequestPath},
			{"request.meta.json", &meta.RequestMetaPath},
			{"upstream_request.json", &meta.UpstreamRequestPath},
			{"upstream_response.json", &meta.UpstreamResponsePath},
			{"response.sse", &meta.ResponsePath},
			{"response.json", &meta.ResponsePath},
			{"response.txt", &meta.ResponsePath},
			{"response.bin", &meta.ResponsePath},
		}
		for _, path := range paths {
			name, target := path.name, path.target
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				*target = name
			}
		}
		encoded, err := marshalJSON(meta)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(metadataPath, encoded, 0o600); err != nil {
			return err
		}
	}
	return nil
}

type archiveDir struct {
	scope string
	id    int
	name  string
}

func listScopedNumericDirs(root string) ([]archiveDir, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	dirs := make([]archiveDir, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scope := entry.Name()
		if id, err := strconv.Atoi(scope); err == nil {
			dirs = append(dirs, archiveDir{scope: "", id: id, name: scope})
			continue
		}
		scopeEntries, err := os.ReadDir(filepath.Join(root, scope))
		if err != nil {
			return nil, err
		}
		for _, roundEntry := range scopeEntries {
			if !roundEntry.IsDir() {
				continue
			}
			id, err := strconv.Atoi(roundEntry.Name())
			if err != nil {
				continue
			}
			dirs = append(dirs, archiveDir{scope: scope, id: id, name: roundEntry.Name()})
		}
	}
	return dirs, nil
}

func nextSequence(root string) (int, error) {
	dirs, err := listScopedNumericDirs(root)
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, dir := range dirs {
		if dir.id >= maxID {
			maxID = dir.id
		}
	}
	return maxID + 1, nil
}

func writeJSONOrRaw(path string, body []byte) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return writeFileAtomic(path, body, 0o600)
	}
	encoded, err := marshalJSON(value)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, encoded, 0o600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".archive-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	if dirHandle, openErr := os.Open(dir); openErr == nil {
		err = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return err
}

func marshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
