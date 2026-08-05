package store

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	events "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	"ai-proxy/internal/pkg/aiproxystate"
)

type indexEntry struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	CreatedAt string `json:"created_at"`
}

type Store struct {
	mu        sync.Mutex
	root      string
	documents *aiproxystate.Documents
	index     map[string]indexEntry
	tags      map[string][]string
}

// Open creates the image-store owner's state store and reports startup errors.
func Open(root, databasePath, memoryLimit string, threads int) (*Store, error) {
	s := &Store{
		root:  root,
		index: map[string]indexEntry{},
		tags:  map[string][]string{},
	}
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	s.documents = documents
	if err := s.loadIndex(); err != nil {
		_ = documents.Close()
		return nil, fmt.Errorf("load image index: %w", err)
	}
	if err := s.loadTags(); err != nil {
		_ = documents.Close()
		return nil, fmt.Errorf("load image tags: %w", err)
	}
	return s, nil
}

// New is retained for direct package tests. Production startup must call Open.
func New(root string) *Store {
	s, err := Open(root, filepath.Join(root, "ai-proxy.duckdb"), "128MB", 1)
	if err != nil {
		panic(err)
	}
	return s
}

func (s *Store) loadTags() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	raw, err := s.documents.LoadImageTags()
	if err != nil {
		return err
	}
	for path, tags := range raw {
		path = safeRel(path)
		if path == "" {
			continue
		}
		if cleaned := cleanTags(tags); len(cleaned) > 0 {
			s.tags[path] = cleaned
		}
	}
	return nil
}

func (s *Store) saveTagsLocked() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	return s.documents.ReplaceImageTags(s.tags)
}

func (s *Store) loadIndex() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	rows, err := s.documents.LoadImages()
	if err != nil {
		return err
	}
	for _, row := range rows {
		path := safeRel(row.Path)
		if path == "" {
			continue
		}
		s.index[path] = indexEntry{Path: path, Size: row.Size, Width: row.Width, Height: row.Height, CreatedAt: row.CreatedAt}
	}
	return nil
}

func (s *Store) saveIndexLocked() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	rows := make([]aiproxystate.ImageRow, 0, len(s.index))
	for path, entry := range s.index {
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		rows = append(rows, aiproxystate.ImageRow{Path: path, Size: entry.Size, Width: entry.Width, Height: entry.Height, CreatedAt: entry.CreatedAt, Payload: payload})
	}
	return s.documents.ReplaceImages(rows)
}

func (s *Store) Close() error {
	if s == nil || s.documents == nil {
		return nil
	}
	return s.documents.Close()
}

func (s *Store) Save(payload []byte, baseURL string) (events.SaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	sum := md5.Sum(payload)
	rel := filepath.ToSlash(filepath.Join(
		fmt.Sprintf("%04d", now.Year()),
		fmt.Sprintf("%02d", now.Month()),
		fmt.Sprintf("%02d", now.Day()),
		fmt.Sprintf("%d_%s.png", now.UnixMilli(), hex.EncodeToString(sum[:8])),
	))
	full := filepath.Join(s.root, "images", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return events.SaveResult{}, err
	}
	if err := os.WriteFile(full, payload, 0o644); err != nil {
		return events.SaveResult{}, err
	}
	w, h := imageDims(payload)
	entry := indexEntry{
		Path:      rel,
		Size:      int64(len(payload)),
		Width:     w,
		Height:    h,
		CreatedAt: now.Format(time.RFC3339),
	}
	s.index[rel] = entry
	if err := s.saveIndexLocked(); err != nil {
		return events.SaveResult{}, err
	}
	return events.SaveResult{
		RelativePath: rel,
		PublicURL:    publicURL(baseURL, rel),
		Width:        w,
		Height:       h,
		Size:         len(payload),
	}, nil
}

func (s *Store) GetBytes(rel string) ([]byte, error) {
	rel = safeRel(rel)
	full := filepath.Join(s.root, "images", filepath.FromSlash(rel))
	return os.ReadFile(full)
}

func (s *Store) Exists(rel string) bool {
	rel = safeRel(rel)
	full := filepath.Join(s.root, "images", filepath.FromSlash(rel))
	_, err := os.Stat(full)
	return err == nil
}

// EnsureThumbnail creates a PNG thumbnail whose longest side is at most 320
// pixels. It uses a deterministic in-process scaler so image management has
// no external image-service dependency.
func (s *Store) EnsureThumbnail(rel, baseURL string) (events.EnsureThumbnailResult, error) {
	rel = safeRel(rel)
	if rel == "" {
		return events.EnsureThumbnailResult{}, fmt.Errorf("invalid image path")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source := filepath.Join(s.root, "images", filepath.FromSlash(rel))
	info, err := os.Stat(source)
	if err != nil {
		return events.EnsureThumbnailResult{}, err
	}
	targetRel := rel + ".png"
	target := filepath.Join(s.root, "image_thumbnails", filepath.FromSlash(targetRel))
	if thumbInfo, statErr := os.Stat(target); statErr == nil && !thumbInfo.ModTime().Before(info.ModTime()) {
		return events.EnsureThumbnailResult{ThumbnailPath: targetRel, URL: publicThumbURL(baseURL, rel)}, nil
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return events.EnsureThumbnailResult{}, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		return events.EnsureThumbnailResult{}, fmt.Errorf("decode image thumbnail: %w", err)
	}
	thumbnail := scaleThumbnail(decoded, 320)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return events.EnsureThumbnailResult{}, err
	}
	tmp := target + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return events.EnsureThumbnailResult{}, err
	}
	encodeErr := png.Encode(file, thumbnail)
	closeErr := file.Close()
	if encodeErr != nil {
		_ = os.Remove(tmp)
		return events.EnsureThumbnailResult{}, encodeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return events.EnsureThumbnailResult{}, closeErr
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return events.EnsureThumbnailResult{}, err
	}
	return events.EnsureThumbnailResult{ThumbnailPath: targetRel, URL: publicThumbURL(baseURL, rel)}, nil
}

func (s *Store) GetThumbnailBytes(rel string) ([]byte, error) {
	rel = safeRel(rel)
	if rel == "" {
		return nil, fmt.Errorf("invalid image path")
	}
	if _, err := s.EnsureThumbnail(rel, ""); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(s.root, "image_thumbnails", filepath.FromSlash(rel)+".png"))
}

func (s *Store) Delete(paths []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for _, p := range paths {
		rel := safeRel(p)
		full := filepath.Join(s.root, "images", filepath.FromSlash(rel))
		if err := os.Remove(full); err == nil || os.IsNotExist(err) {
			if err == nil {
				deleted++
			}
			delete(s.index, rel)
			delete(s.tags, rel)
			// thumbnail
			_ = os.Remove(filepath.Join(s.root, "image_thumbnails", filepath.FromSlash(rel)+".png"))
		}
	}
	if err := s.saveIndexLocked(); err != nil {
		return deleted, err
	}
	if err := s.saveTagsLocked(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (s *Store) List(baseURL, startDate, endDate string) []events.ImageItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.ImageItem, 0, len(s.index))
	for _, e := range s.index {
		if startDate != "" && e.CreatedAt < startDate {
			continue
		}
		if endDate != "" && e.CreatedAt > endDate+"T23:59:59Z" {
			continue
		}
		name := filepath.Base(e.Path)
		date := ""
		parts := strings.Split(e.Path, "/")
		if len(parts) >= 3 {
			date = strings.Join(parts[:3], "-")
		}
		out = append(out, events.ImageItem{
			Path:         e.Path,
			Name:         name,
			Date:         date,
			Size:         e.Size,
			URL:          publicURL(baseURL, e.Path),
			ThumbnailURL: publicThumbURL(baseURL, e.Path),
			CreatedAt:    e.CreatedAt,
			Width:        e.Width,
			Height:       e.Height,
			Tags:         append([]string(nil), s.tags[e.Path]...),
		})
	}
	return out
}

func (s *Store) ListTags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]struct{}{}
	for _, tags := range s.tags {
		for _, tag := range tags {
			seen[tag] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for tag := range seen {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result
}

func (s *Store) SetTags(path string, tags []string) ([]string, error) {
	path = safeRel(path)
	if path == "" {
		return nil, fmt.Errorf("invalid image path")
	}
	cleaned := cleanTags(tags)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(cleaned) == 0 {
		delete(s.tags, path)
	} else {
		s.tags[path] = cleaned
	}
	if err := s.saveTagsLocked(); err != nil {
		return nil, err
	}
	return append([]string(nil), cleaned...), nil
}

func (s *Store) DeleteTag(tag string) (int, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return 0, fmt.Errorf("tag is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for path, tags := range s.tags {
		updated := make([]string, 0, len(tags))
		changed := false
		for _, item := range tags {
			if item == tag {
				changed = true
				continue
			}
			updated = append(updated, item)
		}
		if !changed {
			continue
		}
		removed++
		if len(updated) == 0 {
			delete(s.tags, path)
		} else {
			s.tags[path] = updated
		}
	}
	if removed > 0 {
		if err := s.saveTagsLocked(); err != nil {
			return 0, err
		}
	}
	return removed, nil
}

func (s *Store) StorageStats() (events.StorageStatsResult, error) {
	imagesRoot := filepath.Join(s.root, "images")
	if err := os.MkdirAll(imagesRoot, 0o755); err != nil {
		return events.StorageStatsResult{}, err
	}
	total, free, err := diskSpace(imagesRoot)
	if err != nil {
		return events.StorageStatsResult{}, err
	}
	var count int
	var size int64
	if err := filepath.WalkDir(imagesRoot, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			count++
			size += info.Size()
		}
		return nil
	}); err != nil {
		return events.StorageStatsResult{}, err
	}
	const megabyte = int64(1024 * 1024)
	return events.StorageStatsResult{
		DiskTotalMB:    total / megabyte,
		DiskUsedMB:     (total - free) / megabyte,
		DiskFreeMB:     free / megabyte,
		ImageCount:     count,
		ImageSizeMB:    size / megabyte,
		ImageSizeBytes: size,
	}, nil
}

func (s *Store) CompressImages() (events.CompressResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	imagesRoot := filepath.Join(s.root, "images")
	var result events.CompressResult
	changedIndex := false
	err := filepath.WalkDir(imagesRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".png") {
			return nil
		}
		original, err := os.ReadFile(path)
		if err != nil {
			return nil // one unreadable image must not abort administrative maintenance
		}
		decoded, _, err := image.Decode(bytes.NewReader(original))
		if err != nil {
			return nil
		}
		var compressed bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		if err := encoder.Encode(&compressed, decoded); err != nil || compressed.Len() >= len(original) {
			return nil
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, compressed.Bytes(), 0o644); err != nil {
			return nil
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return nil
		}
		result.Compressed++
		result.SavedBytes += int64(len(original) - compressed.Len())
		rel, err := filepath.Rel(imagesRoot, path)
		if err == nil {
			rel = filepath.ToSlash(rel)
			if item, exists := s.index[rel]; exists {
				item.Size = int64(compressed.Len())
				s.index[rel] = item
				changedIndex = true
			}
		}
		return nil
	})
	if err != nil {
		return events.CompressResult{}, err
	}
	if changedIndex {
		if err := s.saveIndexLocked(); err != nil {
			return events.CompressResult{}, err
		}
	}
	result.SavedMB = result.SavedBytes / (1024 * 1024)
	return result, nil
}

func (s *Store) CleanupToTarget(targetFreeMB int64, dryRun bool) (events.CleanupToTargetResult, error) {
	if targetFreeMB < 0 {
		return events.CleanupToTargetResult{}, fmt.Errorf("target_free_mb must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	imagesRoot := filepath.Join(s.root, "images")
	if err := os.MkdirAll(imagesRoot, 0o755); err != nil {
		return events.CleanupToTargetResult{}, err
	}
	_, free, err := diskSpace(imagesRoot)
	if err != nil {
		return events.CleanupToTargetResult{}, err
	}
	const megabyte = int64(1024 * 1024)
	currentFreeMB := free / megabyte
	result := events.CleanupToTargetResult{TargetFreeMB: targetFreeMB, CurrentFreeMB: currentFreeMB, DryRun: dryRun}
	if currentFreeMB >= targetFreeMB {
		result.Done = true
		return result, nil
	}
	type candidate struct {
		path string
		rel  string
		size int64
		mod  time.Time
	}
	files := make([]candidate, 0)
	if err := filepath.WalkDir(imagesRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".png") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(imagesRoot, path)
		if err != nil {
			return err
		}
		files = append(files, candidate{path: path, rel: filepath.ToSlash(rel), size: info.Size(), mod: info.ModTime()})
		return nil
	}); err != nil {
		return events.CleanupToTargetResult{}, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	var freedBytes int64
	changed := false
	for _, file := range files {
		if currentFreeMB+freedBytes/megabyte >= targetFreeMB {
			break
		}
		result.Removed++
		freedBytes += file.size
		if dryRun {
			continue
		}
		if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
			continue
		}
		_ = os.Remove(filepath.Join(s.root, "image_thumbnails", filepath.FromSlash(file.rel)+".png"))
		delete(s.index, file.rel)
		delete(s.tags, file.rel)
		changed = true
	}
	if changed {
		if err := s.saveIndexLocked(); err != nil {
			return events.CleanupToTargetResult{}, err
		}
		if err := s.saveTagsLocked(); err != nil {
			return events.CleanupToTargetResult{}, err
		}
	}
	result.FreedMB = freedBytes / megabyte
	result.CurrentFreeMB = currentFreeMB + result.FreedMB
	result.Done = result.CurrentFreeMB >= targetFreeMB
	return result, nil
}

func (s *Store) AbsolutePath(rel string) string {
	return filepath.Join(s.root, "images", filepath.FromSlash(safeRel(rel)))
}

func publicURL(baseURL, rel string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return "/images/" + rel
	}
	return baseURL + "/images/" + rel
}

func publicThumbURL(baseURL, rel string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return "/image-thumbnails/" + rel + ".png"
	}
	return baseURL + "/image-thumbnails/" + rel + ".png"
}

func safeRel(rel string) string {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return ""
	}
	return rel
}

func cleanTags(tags []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > 128 {
			tag = tag[:128]
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		result = append(result, tag)
		if len(result) == 64 {
			break
		}
	}
	return result
}

func imageDims(payload []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func scaleThumbnail(source image.Image, maxSide int) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return source
	}
	if width <= maxSide && height <= maxSide {
		return source
	}
	targetWidth, targetHeight := width, height
	if width >= height {
		targetWidth = maxSide
		targetHeight = max(1, height*maxSide/width)
	} else {
		targetHeight = maxSide
		targetWidth = max(1, width*maxSide/height)
	}
	result := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		sourceY := bounds.Min.Y + y*height/targetHeight
		for x := 0; x < targetWidth; x++ {
			sourceX := bounds.Min.X + x*width/targetWidth
			result.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return result
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return value
		}
	}
	return ""
}

func asInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	}
	return 0
}
