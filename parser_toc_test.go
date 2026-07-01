package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests run against real sample epubs in testdata/epubs, which are
// gitignored (never committed) — see CLAUDE.md / .gitignore. They validate
// that chapter numbering derived from the epub's own table of contents
// (extractTOC + applyTOCNumbering) stays correct across genuinely different
// books: one that embeds its own chapter number in the heading text and
// prints it as part of a real-world reader chat bug (We Are Bob), one with
// explicit "Chapter N" TOC labels but no in-body numbering (Building a
// Second Brain), one with bare-digit TOC labels (A Canticle for Leibowitz),
// one with in-story numeric codenames that must NOT be mistaken for chapter
// numbers (There Is No Antimemetics Division, "SCP-055"), and one with
// mixed numbered/unnumbered sections (Project Hail Mary). If these files
// aren't present locally, the tests skip rather than fail.
func loadTestEpub(t *testing.T, filename string) []Chapter {
	t.Helper()
	path := filepath.Join("testdata", "epubs", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("sample epub not present locally (%s) — skipping", path)
	}
	chapters, err := parseEpub(data)
	if err != nil {
		t.Fatalf("parseEpub(%s): %v", filename, err)
	}
	return chapters
}

func TestTOCNumbering_WeAreBob(t *testing.T) {
	chapters := loadTestEpub(t, "We_Are_Bob_We_Are_Legion.epub")

	if got := realChapterCount(chapters); got != 61 {
		t.Errorf("realChapterCount = %d, want 61", got)
	}

	// Regression guard for the originally reported bug: the book's own real
	// chapter 10 — the one whose body text literally opens "10. Bob –
	// August 10, 2133" — must resolve to that exact chapter, not our raw
	// spine position 10 (which is five chapters short, since front matter
	// pushes the real chapter 1 to spine position 6).
	spineUpTo := resolveSpineUpTo(chapters, 10)
	if spineUpTo < 1 || spineUpTo > len(chapters) {
		t.Fatalf("resolveSpineUpTo(10) = %d out of range", spineUpTo)
	}
	got := chapters[spineUpTo-1]
	if got.Title != "Bob – August 10, 2133" {
		t.Errorf("resolveSpineUpTo(10) resolved to spine %d (%q), want the chapter titled %q",
			spineUpTo, got.Title, "Bob – August 10, 2133")
	}
	if got.Number != 10 {
		t.Errorf("chapter at resolved spine position has Number=%d, want 10", got.Number)
	}
	if chapterDisplayNumber(got) != 10 {
		t.Errorf("chapterDisplayNumber = %d, want 10", chapterDisplayNumber(got))
	}

	// The embedded numbering continues straight through the "Part 2"
	// divider after the ship launches (verified against the actual epub
	// text) rather than resetting — real chapter 15 is eleven years later,
	// at a different star system, not more pre-launch Earth content. This
	// guards against silently reintroducing an assumption that numbering
	// resets at part boundaries.
	spine15 := resolveSpineUpTo(chapters, 15)
	if title := chapters[spine15-1].Title; title != "Bob – September 2144 – Epsilon Eridani" {
		t.Errorf("resolveSpineUpTo(15) resolved to %q, want %q — did part-boundary numbering change?",
			title, "Bob – September 2144 – Epsilon Eridani")
	}
}

func TestTOCNumbering_BuildingASecondBrain(t *testing.T) {
	chapters := loadTestEpub(t, "Building_a_Second_Brain.epub")

	if got := realChapterCount(chapters); got != 10 {
		t.Errorf("realChapterCount = %d, want 10", got)
	}

	// "Chapter 1" must resolve to real chapter number 1, not its raw spine
	// position (which is later, since an unnumbered Introduction precedes
	// it in the TOC) — this is the exact bug applyTOCNumbering's per-entry
	// label matching (rather than sequential TOC position) is meant to avoid.
	spineUpTo := resolveSpineUpTo(chapters, 1)
	got := chapters[spineUpTo-1]
	if got.Number != 1 {
		t.Errorf("resolveSpineUpTo(1) resolved to Number=%d, want 1 (title=%q)", got.Number, got.Title)
	}
	if got.Title == "" || got.Title == "Chapter 0" {
		t.Errorf("chapter 1 title looks wrong: %q", got.Title)
	}
}

func TestTOCNumbering_ACanticleForLeibowitz(t *testing.T) {
	chapters := loadTestEpub(t, "A_Canticle_For_Leibowitz.epub")

	// Bare-digit TOC labels ("1", "2", "3"...) must still be recognized.
	if got := realChapterCount(chapters); got < 25 {
		t.Errorf("realChapterCount = %d, want at least 25 (bare-digit TOC labels should be recognized)", got)
	}
}

func TestTOCNumbering_AntimemeticsDivision(t *testing.T) {
	chapters := loadTestEpub(t, "There_Is_No_Antimemetics_Division.epub")

	// This book has no numbering convention at all, and — critically — a
	// chapter titled "SCP-055: [unknown]" whose embedded number must NOT be
	// mistaken for a chapter number (a false positive here would silently
	// misroute every later chapter selection).
	for _, ch := range chapters {
		if ch.Title == "SCP-055: [unknown]" && ch.Number == 55 {
			t.Errorf("chapter %q was assigned Number=55, likely misread from its in-story codename rather than a real chapter number", ch.Title)
		}
	}
}

func TestTOCNumbering_ProjectHailMary(t *testing.T) {
	chapters := loadTestEpub(t, "Project_Hail_Mary.epub")

	if got := realChapterCount(chapters); got < 25 {
		t.Errorf("realChapterCount = %d, want at least 25", got)
	}

	// Regression guard for the title-vs-number mismatch bug: a chapter
	// assigned a real TOC-derived Number must not keep a stale
	// auto-generated "Chapter N" title using the wrong N (e.g. "Chapter 1
	// (Chapter 3)" — confusing and self-contradictory).
	spineUpTo := resolveSpineUpTo(chapters, 1)
	got := chapters[spineUpTo-1]
	if got.Number == 1 && got.Title != "" {
		wantAutoTitle := "Chapter 3" // the stale spine-position-based title this chapter would have had before the fix
		if got.Title == wantAutoTitle {
			t.Errorf("chapter with Number=1 kept stale auto-generated title %q instead of being reconciled to match", got.Title)
		}
	}
}
