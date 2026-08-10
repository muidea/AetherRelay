package admin

import (
	"mime"
	"testing"
	"time"
)

func TestBundleExportFilenameUsesUTCAndStableProfile(t *testing.T) {
	exportedAt := time.Date(2026, time.August, 10, 20, 34, 56, 987654321, time.FixedZone("CST", 8*60*60))
	want := "ai-proxy-provider-bundle-v1-safe-20260810T123456Z.json"
	if got := bundleExportFilename(bundleExportArtifactProvider, 1, bundleExportProfileSafe, exportedAt); got != want {
		t.Fatalf("filename=%q, want %q", got, want)
	}

	disposition := bundleExportContentDisposition(bundleExportArtifactProvider, 1, bundleExportProfileSafe, exportedAt)
	mediaType, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		t.Fatalf("parse Content-Disposition=%q: %v", disposition, err)
	}
	if mediaType != "attachment" || params["filename"] != want {
		t.Fatalf("Content-Disposition=%q, media_type=%q params=%v", disposition, mediaType, params)
	}
}

func TestBundleExportFilenameDistinguishesProfilesAndArtifacts(t *testing.T) {
	exportedAt := time.Date(2026, time.August, 10, 12, 34, 56, 0, time.UTC)
	if safe, complete := bundleExportFilename(bundleExportArtifactProvider, 1, bundleExportProfileSafe, exportedAt), bundleExportFilename(bundleExportArtifactProvider, 1, bundleExportProfileComplete, exportedAt); safe == complete {
		t.Fatalf("safe and complete filenames unexpectedly match: %q", safe)
	}
	if got := bundleExportFilename(bundleExportArtifactAccountPool, 2, bundleExportProfileComplete, exportedAt); got != "ai-proxy-account-pool-bundle-v2-complete-20260810T123456Z.json" {
		t.Fatalf("account-pool filename=%q", got)
	}
}
