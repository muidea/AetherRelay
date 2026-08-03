package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	events "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	"ai-proxy/internal/pkg/aiproxystate"
	"ai-proxy/internal/pkg/chatgpttokenusage"
)

type taskRecord struct {
	events.TaskView
	OwnerID    string  `json:"owner_id"`
	AccountID  string  `json:"account_id,omitempty"`
	Key        string  `json:"-"`
	StartedTS  float64 `json:"started_ts,omitempty"`
	CreatedTS  float64 `json:"created_ts,omitempty"`
	rawData    json.RawMessage
	rawUsage   json.RawMessage
	extra      map[string]json.RawMessage
	dataDirty  bool
	usageDirty bool
}

// UnmarshalJSON keeps Python-only task fields local to the persistence owner.
// EventHub callers see only TaskView's bounded projection, while a recovery or
// later Go task write cannot erase data the Go owner does not understand yet.
func (r *taskRecord) UnmarshalJSON(data []byte) error {
	type wire struct {
		events.TaskView
		OwnerID   string  `json:"owner_id"`
		AccountID string  `json:"account_id,omitempty"`
		StartedTS float64 `json:"started_ts,omitempty"`
		CreatedTS float64 `json:"created_ts,omitempty"`
	}
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	r.TaskView = decoded.TaskView
	r.OwnerID = decoded.OwnerID
	r.AccountID = decoded.AccountID
	r.StartedTS = decoded.StartedTS
	r.CreatedTS = decoded.CreatedTS

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	r.rawData = cloneRaw(fields["data"])
	r.rawUsage = cloneRaw(fields["usage"])
	r.extra = make(map[string]json.RawMessage)
	for key, value := range fields {
		if !knownTaskField(key) {
			r.extra[key] = cloneRaw(value)
		}
	}
	return nil
}

func (r taskRecord) MarshalJSON() ([]byte, error) {
	type wire struct {
		events.TaskView
		OwnerID   string  `json:"owner_id"`
		AccountID string  `json:"account_id,omitempty"`
		StartedTS float64 `json:"started_ts,omitempty"`
		CreatedTS float64 `json:"created_ts,omitempty"`
	}
	data, err := json.Marshal(wire{
		TaskView:  r.TaskView,
		OwnerID:   r.OwnerID,
		AccountID: r.AccountID,
		StartedTS: r.StartedTS,
		CreatedTS: r.CreatedTS,
	})
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if len(r.rawData) > 0 && !r.dataDirty {
		fields["data"] = cloneRaw(r.rawData)
	}
	if len(r.rawUsage) > 0 && !r.usageDirty {
		fields["usage"] = cloneRaw(r.rawUsage)
	}
	for key, value := range r.extra {
		fields[key] = cloneRaw(value)
	}
	return json.Marshal(fields)
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

func knownTaskField(key string) bool {
	switch key {
	case "id", "status", "mode", "model", "provider", "prompt", "size", "quality", "created_at", "updated_at", "conversation_id", "data", "error", "progress", "elapsed_secs", "duration_ms", "usage", "owner_id", "account_id", "started_ts", "created_ts":
		return true
	default:
		return false
	}
}

type Store struct {
	mu        sync.Mutex
	documents *aiproxystate.Documents
	items     map[string]taskRecord // key = owner:taskID
}

// Open creates the image-task owner's state store and reports startup errors.
func Open(databasePath, memoryLimit string, threads int) (*Store, error) {
	s := &Store{items: map[string]taskRecord{}}
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	s.documents = documents
	if err := s.load(); err != nil {
		_ = documents.Close()
		return nil, fmt.Errorf("load image task state: %w", err)
	}
	if s.recoverUnfinishedLocked() {
		if err := s.saveLocked(); err != nil {
			_ = documents.Close()
			return nil, fmt.Errorf("recover image task state: %w", err)
		}
	}
	return s, nil
}

// New is retained for direct package tests. Production startup must call Open.
func New(databasePath string) *Store {
	s, err := Open(databasePath, "128MB", 1)
	if err != nil {
		panic(err)
	}
	return s
}

func taskKey(ownerID, taskID string) string {
	return ownerID + ":" + taskID
}

func (s *Store) load() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	rows, err := s.documents.LoadImageTasks()
	if err != nil {
		return err
	}
	for _, row := range rows {
		var task taskRecord
		if err := json.Unmarshal(row.Payload, &task); err != nil {
			return fmt.Errorf("decode image task %s:%s: %w", row.OwnerID, row.TaskID, err)
		}
		task.OwnerID = row.OwnerID
		task.ID = row.TaskID
		task.Key = taskKey(row.OwnerID, row.TaskID)
		s.items[task.Key] = task
	}
	return nil
}

func (s *Store) saveLocked() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	keys := make([]string, 0, len(s.items))
	for key := range s.items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]aiproxystate.ImageTaskRow, 0, len(keys))
	for _, key := range keys {
		task := s.items[key]
		payload, err := json.Marshal(task)
		if err != nil {
			return err
		}
		rows = append(rows, aiproxystate.ImageTaskRow{OwnerID: task.OwnerID, TaskID: task.ID, Payload: payload})
	}
	return s.documents.ReplaceImageTasks(rows)
}

func (s *Store) Close() error {
	if s == nil || s.documents == nil {
		return nil
	}
	return s.documents.Close()
}

func (s *Store) recoverUnfinishedLocked() bool {
	changed := false
	now := time.Now().UTC().Format(time.RFC3339)
	for k, t := range s.items {
		if t.Status == events.StatusQueued || t.Status == events.StatusRunning {
			t.Status = events.StatusError
			t.Error = "服务已重启，未完成的图片任务已中断"
			t.UpdatedAt = now
			t.Progress = ""
			s.items[k] = t
			changed = true
		}
	}
	return changed
}

func (s *Store) GetOrCreateGeneration(ownerID, clientTaskID, prompt, model, size, quality string) (events.TaskView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, clientTaskID)
	if existing, ok := s.items[key]; ok {
		return existing.TaskView, false, nil
	}
	now := time.Now().UTC()
	rec := taskRecord{
		TaskView: events.TaskView{
			ID:        clientTaskID,
			Status:    events.StatusQueued,
			Mode:      "generate",
			Model:     model,
			Prompt:    prompt,
			Size:      size,
			Quality:   quality,
			CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339),
			Progress:  "queued",
		},
		OwnerID:   ownerID,
		Key:       key,
		CreatedTS: float64(now.Unix()),
	}
	s.items[key] = rec
	if err := s.saveLocked(); err != nil {
		return events.TaskView{}, false, err
	}
	return rec.TaskView, true, nil
}

func (s *Store) GetOrCreateEdit(ownerID, clientTaskID, prompt, model, size, quality string) (events.TaskView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, clientTaskID)
	if existing, ok := s.items[key]; ok {
		return existing.TaskView, false, nil
	}
	now := time.Now().UTC()
	rec := taskRecord{
		TaskView: events.TaskView{
			ID:        clientTaskID,
			Status:    events.StatusQueued,
			Mode:      "edit",
			Model:     model,
			Prompt:    prompt,
			Size:      size,
			Quality:   quality,
			CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339),
			Progress:  "queued",
		},
		OwnerID:   ownerID,
		Key:       key,
		CreatedTS: float64(now.Unix()),
	}
	s.items[key] = rec
	if err := s.saveLocked(); err != nil {
		return events.TaskView{}, false, err
	}
	return rec.TaskView, true, nil
}

func (s *Store) MarkRunning(ownerID, taskID, progress string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return
	}
	now := time.Now().UTC()
	rec.Status = events.StatusRunning
	rec.Progress = progress
	rec.UpdatedAt = now.Format(time.RFC3339)
	if rec.StartedTS == 0 {
		rec.StartedTS = float64(now.Unix())
	}
	s.items[key] = rec
	_ = s.saveLocked()
}

func (s *Store) MarkProgress(ownerID, taskID, progress string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return
	}
	rec.Progress = progress
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.items[key] = rec
	_ = s.saveLocked()
}

// SetAccountID records a stable account reference for later conversation
// recovery. The account token remains exclusively owned by account storage.
func (s *Store) SetAccountID(ownerID, taskID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return
	}
	rec.AccountID = strings.TrimSpace(accountID)
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.items[key] = rec
	_ = s.saveLocked()
}

func (s *Store) SetProvider(ownerID, taskID, provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return
	}
	rec.Provider = strings.TrimSpace(provider)
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.items[key] = rec
	_ = s.saveLocked()
}

func (s *Store) MarkSuccess(ownerID, taskID string, data []events.ImageData, conversationID string, usage *tokenusage.Usage, durationMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return
	}
	rec.Status = events.StatusSuccess
	rec.Data = data
	rec.dataDirty = true
	rec.ConversationID = conversationID
	rec.Usage = usage
	rec.usageDirty = true
	rec.DurationMs = durationMs
	rec.Error = ""
	rec.Progress = ""
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.items[key] = rec
	_ = s.saveLocked()
}

func (s *Store) MarkError(ownerID, taskID, errMsg, conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return
	}
	rec.Status = events.StatusError
	rec.Error = errMsg
	rec.ConversationID = conversationID
	rec.Data = []events.ImageData{}
	rec.dataDirty = true
	rec.Progress = ""
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.items[key] = rec
	_ = s.saveLocked()
}

// RetryGeneration resets a terminal generation task. Eligibility belongs to
// the image-task biz, which understands the upstream failure stage.
func (s *Store) RetryGeneration(ownerID, taskID string) (events.TaskView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(ownerID, taskID)
	rec, ok := s.items[key]
	if !ok {
		return events.TaskView{}, false, nil
	}
	if rec.Status != events.StatusError || rec.Mode != "generate" || rec.ConversationID != "" {
		return rec.TaskView, false, nil
	}
	rec.Status = events.StatusQueued
	rec.Progress = "retrying_submission"
	rec.Error = ""
	rec.AccountID = ""
	rec.Data = nil
	rec.dataDirty = true
	rec.Usage = nil
	rec.usageDirty = true
	rec.DurationMs = 0
	rec.StartedTS = 0
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.items[key] = rec
	if err := s.saveLocked(); err != nil {
		return events.TaskView{}, false, err
	}
	return rec.TaskView, true, nil
}

func (s *Store) List(ownerID string, taskIDs []string) (items []events.TaskView, missing []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := float64(time.Now().Unix())
	if len(taskIDs) == 0 {
		for _, rec := range s.items {
			if rec.OwnerID != ownerID {
				continue
			}
			items = append(items, withElapsed(rec, now))
		}
		return items, nil
	}
	for _, id := range taskIDs {
		key := taskKey(ownerID, id)
		rec, ok := s.items[key]
		if !ok {
			missing = append(missing, id)
			continue
		}
		items = append(items, withElapsed(rec, now))
	}
	return items, missing
}

func (s *Store) Get(ownerID, taskID string) (events.TaskView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[taskKey(ownerID, taskID)]
	if !ok {
		return events.TaskView{}, false
	}
	return withElapsed(rec, float64(time.Now().Unix())), true
}

// ResumeInfo is private to the image-task owner; it deliberately projects the
// account ID separately from the public task response.
type ResumeInfo struct {
	Task      events.TaskView
	AccountID string
}

func (s *Store) GetResumeInfo(ownerID, taskID string) (ResumeInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[taskKey(ownerID, taskID)]
	if !ok {
		return ResumeInfo{}, false
	}
	return ResumeInfo{Task: withElapsed(rec, float64(time.Now().Unix())), AccountID: rec.AccountID}, true
}

func withElapsed(rec taskRecord, now float64) events.TaskView {
	view := rec.TaskView
	if view.Status == events.StatusQueued && rec.CreatedTS > 0 {
		view.ElapsedSecs = now - rec.CreatedTS
	}
	if view.Status == events.StatusRunning && rec.StartedTS > 0 {
		view.ElapsedSecs = now - rec.StartedTS
	}
	return view
}
