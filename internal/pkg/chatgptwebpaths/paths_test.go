package chatgptwebpaths

import (
	"path/filepath"
	"testing"
)

func TestLayoutPaths(t *testing.T) {
	root := filepath.Join("runtime", "chatgpt")
	if got, want := AccountsFile(root), filepath.Join(root, AccountsFilename); got != want {
		t.Fatalf("accounts file = %q, want %q", got, want)
	}
	if got, want := ImageIndexFile(root), filepath.Join(root, ImageIndexFilename); got != want {
		t.Fatalf("image index = %q, want %q", got, want)
	}
	if got, want := ImageTasksFile(root), filepath.Join(root, ImageTasksFilename); got != want {
		t.Fatalf("image tasks = %q, want %q", got, want)
	}
	if got, want := ImagesDir(root), filepath.Join(root, ImagesDirectory); got != want {
		t.Fatalf("images dir = %q, want %q", got, want)
	}
	if got, want := ImageThumbnailsDir(root), filepath.Join(root, ImageThumbnailsDirname); got != want {
		t.Fatalf("thumbnails dir = %q, want %q", got, want)
	}
}
