package changelog

import (
	"strings"
	"testing"
)

func TestChangesChangelog(t *testing.T) {
	if !ChangesChangelog("diff --git a/CHANGELOG.md b/CHANGELOG.md\n") {
		t.Fatal("expected changelog change")
	}
	if ChangesChangelog("diff --git a/README.md b/README.md\n") {
		t.Fatal("unexpected changelog change")
	}
}

func TestInsertUnreleasedEntryExistingCategory(t *testing.T) {
	content := "# Changelog\n\n## [Unreleased]\n\n### Fixed\n- Old fix\n\n## [1.0.0]\n"
	updated, err := InsertUnreleasedEntry(content, "Fixed", "Prevent timeout.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "### Fixed\n- Prevent timeout.\n- Old fix") {
		t.Fatalf("entry not inserted: %s", updated)
	}
}

func TestInsertUnreleasedEntryCreatesCategory(t *testing.T) {
	content := "# Changelog\n\n## [Unreleased]\n\n## [1.0.0]\n"
	updated, err := InsertUnreleasedEntry(content, "Added", "Expose new endpoint.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "## [Unreleased]\n\n### Added\n\n- Expose new endpoint.") {
		t.Fatalf("category not inserted: %s", updated)
	}
}
