// Package chatgptwebpaths defines the immutable local layout of ChatGPT Web data.
// Resource owners create their own directories lazily; this package has no lifecycle.
package chatgptwebpaths

import "path/filepath"

const (
	AccountsFilename       = "accounts.json"
	ImageIndexFilename     = "image_index.json"
	ImageTasksFilename     = "image_tasks.json"
	ImagesDirectory        = "images"
	ImageThumbnailsDirname = "image_thumbnails"
)

func AccountsFile(root string) string { return filepath.Join(root, AccountsFilename) }

func ImageIndexFile(root string) string { return filepath.Join(root, ImageIndexFilename) }

func ImageTasksFile(root string) string { return filepath.Join(root, ImageTasksFilename) }

func ImagesDir(root string) string { return filepath.Join(root, ImagesDirectory) }

func ImageThumbnailsDir(root string) string { return filepath.Join(root, ImageThumbnailsDirname) }
