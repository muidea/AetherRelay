package store

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestImageIndexPersistsInStateDatabase(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if _, err := s.Save([]byte("Go image payload"), ""); err != nil {
		t.Fatal(err)
	}
	if got := New(dir).List("", "", ""); len(got) != 1 {
		t.Fatalf("items=%#v", got)
	}
}

func TestListReadsStateImageIndex(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.index["external.png"] = indexEntry{Path: "external.png", Size: 123, CreatedAt: "2026-07-26T10:00:00Z"}
	if err := s.saveIndexLocked(); err != nil {
		t.Fatal(err)
	}
	items := New(dir).List("", "", "")
	if len(items) != 1 || items[0].Path != "external.png" || items[0].Size != 123 {
		t.Fatalf("items=%#v", items)
	}
}

func TestEnsureThumbnailScalesAndCachesImage(t *testing.T) {
	imageBytes := bytes.NewBuffer(nil)
	imageValue := image.NewRGBA(image.Rect(0, 0, 640, 320))
	imageValue.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(imageBytes, imageValue); err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())
	saved, err := s.Save(imageBytes.Bytes(), "https://api.example")
	if err != nil {
		t.Fatal(err)
	}
	thumbnail, err := s.EnsureThumbnail(saved.RelativePath, "https://api.example")
	if err != nil {
		t.Fatal(err)
	}
	if thumbnail.URL != "https://api.example/image-thumbnails/"+saved.RelativePath+".png" {
		t.Fatalf("thumbnail=%+v", thumbnail)
	}
	payload, err := os.ReadFile(filepath.Join(s.root, "image_thumbnails", filepath.FromSlash(thumbnail.ThumbnailPath)))
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 320 || config.Height != 160 {
		t.Fatalf("thumbnail size=%dx%d", config.Width, config.Height)
	}
	again, err := s.EnsureThumbnail(saved.RelativePath, "https://api.example")
	if err != nil || again != thumbnail {
		t.Fatalf("cached thumbnail=%+v err=%v", again, err)
	}
	bytes, err := s.GetThumbnailBytes(saved.RelativePath)
	if err != nil || len(bytes) == 0 {
		t.Fatalf("thumbnail bytes=%d err=%v", len(bytes), err)
	}
}

func TestTagsPersistAppearInImageListAndAreRemovedWithImage(t *testing.T) {
	s := New(t.TempDir())
	saved, err := s.Save([]byte("image payload"), "")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := s.SetTags(saved.RelativePath, []string{" product ", "featured", "product", ""})
	if err != nil || len(tags) != 2 || tags[0] != "product" || tags[1] != "featured" {
		t.Fatalf("tags=%#v err=%v", tags, err)
	}
	items := s.List("", "", "")
	if len(items) != 1 || len(items[0].Tags) != 2 || items[0].Tags[0] != "product" {
		t.Fatalf("items=%#v", items)
	}
	if all := s.ListTags(); len(all) != 2 || all[0] != "featured" || all[1] != "product" {
		t.Fatalf("all tags=%#v", all)
	}
	if deleted, err := s.Delete([]string{saved.RelativePath}); err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if all := s.ListTags(); len(all) != 0 {
		t.Fatalf("tags were not removed with image: %#v", all)
	}
	if reloaded := New(s.root); len(reloaded.ListTags()) != 0 {
		t.Fatalf("stale tags after reload: %#v", reloaded.ListTags())
	}
}

func TestStorageStatsCountsLocalImageBytes(t *testing.T) {
	s := New(t.TempDir())
	saved, err := s.Save([]byte("local image payload"), "")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.StorageStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.ImageCount != 1 || stats.ImageSizeBytes != int64(saved.Size) || stats.DiskTotalMB <= 0 || stats.DiskFreeMB < 0 {
		t.Fatalf("stats=%#v", stats)
	}
}

func TestCleanupToTargetSupportsDryRunAndRemovesImageMetadata(t *testing.T) {
	s := New(t.TempDir())
	saved, err := s.Save([]byte("cleanup candidate"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTags(saved.RelativePath, []string{"temporary"}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.StorageStats()
	if err != nil {
		t.Fatal(err)
	}
	target := stats.DiskFreeMB + 1
	dryRun, err := s.CleanupToTarget(target, true)
	if err != nil || dryRun.Removed != 1 || !s.Exists(saved.RelativePath) {
		t.Fatalf("dryRun=%#v exists=%v err=%v", dryRun, s.Exists(saved.RelativePath), err)
	}
	actual, err := s.CleanupToTarget(target, false)
	if err != nil || actual.Removed != 1 || s.Exists(saved.RelativePath) || len(s.ListTags()) != 0 {
		t.Fatalf("actual=%#v exists=%v tags=%#v err=%v", actual, s.Exists(saved.RelativePath), s.ListTags(), err)
	}
}

func TestCompressImagesLeavesValidImageReadable(t *testing.T) {
	imageBytes := bytes.NewBuffer(nil)
	if err := png.Encode(imageBytes, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())
	saved, err := s.Save(imageBytes.Bytes(), "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.CompressImages()
	if err != nil || result.SavedBytes < 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := s.GetBytes(saved.RelativePath); err != nil {
		t.Fatalf("compressed image not readable: %v", err)
	}
}
