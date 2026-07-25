// Package events defines the ChatGPT image-store owner's typed EventHub contract.
package events

const (
	TopicSave            = "aiproxy.chatgpt.imagestore.command.save"
	TopicGetBytes        = "aiproxy.chatgpt.imagestore.command.get_bytes"
	TopicDelete          = "aiproxy.chatgpt.imagestore.command.delete"
	TopicList            = "aiproxy.chatgpt.imagestore.command.list"
	TopicEnsureThumbnail = "aiproxy.chatgpt.imagestore.command.ensure_thumbnail"
	TopicGetThumbnail    = "aiproxy.chatgpt.imagestore.command.get_thumbnail"
	TopicExists          = "aiproxy.chatgpt.imagestore.command.exists"
	TopicListTags        = "aiproxy.chatgpt.imagestore.command.list_tags"
	TopicSetTags         = "aiproxy.chatgpt.imagestore.command.set_tags"
	TopicDeleteTag       = "aiproxy.chatgpt.imagestore.command.delete_tag"
	TopicStorageStats    = "aiproxy.chatgpt.imagestore.command.storage_stats"
	TopicCompress        = "aiproxy.chatgpt.imagestore.command.compress"
	TopicCleanupToTarget = "aiproxy.chatgpt.imagestore.command.cleanup_to_target"
)

type SaveCommand struct {
	Bytes   []byte
	BaseURL string
}
type SaveResult struct {
	RelativePath string
	PublicURL    string
	Width        int
	Height       int
	Size         int
}
type GetBytesCommand struct{ RelativePath string }
type GetBytesResult struct{ Bytes []byte }
type DeleteCommand struct{ Paths []string }
type DeleteResult struct{ Deleted int }
type ListCommand struct {
	BaseURL   string
	StartDate string
	EndDate   string
}
type ImageItem struct {
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	Date         string   `json:"date,omitempty"`
	Size         int64    `json:"size"`
	URL          string   `json:"url"`
	ThumbnailURL string   `json:"thumbnail_url,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	Width        int      `json:"width,omitempty"`
	Height       int      `json:"height,omitempty"`
	Tags         []string `json:"tags"`
}
type ListResult struct{ Items []ImageItem }
type EnsureThumbnailCommand struct{ RelativePath string }
type EnsureThumbnailResult struct {
	ThumbnailPath string
	URL           string
}
type GetThumbnailCommand struct{ RelativePath string }
type GetThumbnailResult struct{ Bytes []byte }
type ExistsCommand struct{ RelativePath string }
type ExistsResult struct{ Exists bool }
type ListTagsCommand struct{}
type ListTagsResult struct {
	Tags []string `json:"tags"`
}
type SetTagsCommand struct {
	Path string   `json:"path"`
	Tags []string `json:"tags"`
}
type SetTagsResult struct {
	Tags []string `json:"tags"`
}
type DeleteTagCommand struct {
	Tag string `json:"tag"`
}
type DeleteTagResult struct {
	RemovedFrom int `json:"removed_from"`
}
type StorageStatsCommand struct{}
type StorageStatsResult struct {
	DiskTotalMB    int64 `json:"disk_total_mb"`
	DiskUsedMB     int64 `json:"disk_used_mb"`
	DiskFreeMB     int64 `json:"disk_free_mb"`
	ImageCount     int   `json:"image_count"`
	ImageSizeMB    int64 `json:"image_size_mb"`
	ImageSizeBytes int64 `json:"image_size_bytes"`
}
type CompressCommand struct{}
type CompressResult struct {
	Compressed int   `json:"compressed"`
	SavedBytes int64 `json:"saved_bytes"`
	SavedMB    int64 `json:"saved_mb"`
}
type CleanupToTargetCommand struct {
	TargetFreeMB int64 `json:"target_free_mb"`
	DryRun       bool  `json:"dry_run"`
}
type CleanupToTargetResult struct {
	Removed       int   `json:"removed"`
	FreedMB       int64 `json:"freed_mb"`
	TargetFreeMB  int64 `json:"target_free_mb"`
	CurrentFreeMB int64 `json:"current_free_mb"`
	Done          bool  `json:"done"`
	DryRun        bool  `json:"dry_run"`
}
