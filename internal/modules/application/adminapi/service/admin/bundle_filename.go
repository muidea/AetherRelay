package admin

import (
	"fmt"
	"mime"
	"time"
)

const (
	bundleExportArtifactProvider    = "provider"
	bundleExportArtifactAccountPool = "account-pool"
	bundleExportProfileSafe         = "safe"
	bundleExportProfileComplete     = "complete"
	bundleExportTimestampLayout     = "20060102T150405Z"
)

// bundleExportFilename is the shared download-name contract for migration
// bundles. The timestamp is UTC and filesystem-safe so browser downloads and
// direct Admin API clients receive the same recognizable artifact name.
//
// ai-proxy-{artifact}-bundle-v{schema}-{profile}-{YYYYMMDDTHHMMSSZ}.json
func bundleExportFilename(artifact string, schemaVersion int, profile string, exportedAt time.Time) string {
	return fmt.Sprintf(
		"ai-proxy-%s-bundle-v%d-%s-%s.json",
		artifact,
		schemaVersion,
		profile,
		exportedAt.UTC().Format(bundleExportTimestampLayout),
	)
}

func bundleExportContentDisposition(artifact string, schemaVersion int, profile string, exportedAt time.Time) string {
	return mime.FormatMediaType("attachment", map[string]string{
		"filename": bundleExportFilename(artifact, schemaVersion, profile, exportedAt),
	})
}
