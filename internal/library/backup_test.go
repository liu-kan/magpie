package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func freezeBackupClock(t *testing.T) {
	t.Helper()
	old := backupNow
	backupNow = func() time.Time { return time.Date(2026, 10, 12, 7, 0, 0, 123000000, time.UTC) }
	t.Cleanup(func() { backupNow = old })
}

func TestBackupsKeepIndependentChanges(t *testing.T) {
	sandbox(t)
	freezeBackupClock(t)
	first := filepath.Join(t.TempDir(), "config.json")
	second := filepath.Join(t.TempDir(), "config.json")
	write(t, first, "first before")
	write(t, second, "second before")
	a, b := newBackups(), newBackups()
	if err := a.keep("agent", first); err != nil {
		t.Fatal(err)
	}
	write(t, first, "first after")
	if err := a.keep("agent", first); err != nil {
		t.Fatal(err)
	}
	if err := b.keep("agent", second); err != nil {
		t.Fatal(err)
	}
	if a.dir == b.dir {
		t.Fatal("independent changes shared a backup folder when the clock matched")
	}
	for dir, want := range map[string]string{a.dir: "first before", b.dir: "second before"} {
		if got := read(t, filepath.Join(dir, "agent", "config.json")); got != want {
			t.Errorf("backup %s: %q, want %q", dir, got, want)
		}
	}
}

func TestBackupFailureCanRetry(t *testing.T) {
	for _, stage := range []string{"folder", "write"} {
		t.Run(stage, func(t *testing.T) {
			sandbox(t)
			p := filepath.Join(t.TempDir(), "config.json")
			write(t, p, "before")
			b := newBackups()
			blocked := BackupDir()
			if stage == "folder" {
				write(t, blocked, "a file blocks the backup folder")
			} else {
				dir, err := b.folder()
				if err != nil {
					t.Fatal(err)
				}
				blocked = filepath.Join(dir, "agent")
				// Both names are directories, so writing the copy fails even
				// when the colliding base name is kept under its parent.
				for _, dst := range []string{
					filepath.Join(blocked, "config.json"),
					filepath.Join(blocked, filepath.Base(filepath.Dir(p)), "config.json"),
				} {
					if err := os.MkdirAll(dst, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := b.keep("agent", p); err == nil {
				t.Fatal("a blocked backup was accepted")
			}
			if err := os.RemoveAll(blocked); err != nil {
				t.Fatal(err)
			}
			if err := b.keep("agent", p); err != nil {
				t.Fatal(err)
			}
			if got := read(t, filepath.Join(b.dir, "agent", "config.json")); got != "before" {
				t.Fatalf("retry did not keep the file: %q", got)
			}
		})
	}
}

func TestSkillBackupsKeepEachVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		keep func(string) (string, error)
	}{
		{"copy", func(p string) (string, error) { return keepLibrarySkill("pdf", p) }},
		{"update", func(p string) (string, error) { return keepReplaced("pdf", p) }},
		{"agent", func(p string) (string, error) { return setAside("claude", p) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			freezeBackupClock(t)
			p := filepath.Join(t.TempDir(), "pdf")
			var kept []string
			for _, text := range []string{"first", "second"} {
				write(t, filepath.Join(p, "SKILL.md"), text)
				dst, err := tc.keep(p)
				if err != nil {
					t.Fatal(err)
				}
				kept = append(kept, dst)
			}
			root := func(p string) string { return filepath.Dir(filepath.Dir(filepath.Dir(p))) }
			if root(kept[0]) == root(kept[1]) {
				t.Error("independent skill changes shared a backup folder")
			}
			for i, want := range []string{"first", "second"} {
				if got := read(t, filepath.Join(kept[i], "SKILL.md")); got != want {
					t.Errorf("version %d: %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestSkillRemovalBackupsStaySeparate(t *testing.T) {
	sandbox(t)
	freezeBackupClock(t)
	version := "one"
	fakeSkillTarball(t, &version)
	var folders []string
	for _, v := range []string{"one", "two"} {
		ok(t)(InstallSkills("owner/repo", []string{"skills/pdf", "skills/docx"}, nil))
		write(t, filepath.Join(skillDir("pdf"), "forms.md"), "forms "+v)
		ok(t)(RemoveSkills([]string{"pdf", "docx"}))
		matches, err := filepath.Glob(filepath.Join(BackupDir(), "*", "skills", "pdf", "forms.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range matches {
			if read(t, p) == "forms "+v {
				folder := filepath.Dir(filepath.Dir(p))
				if read(t, filepath.Join(folder, "docx", "SKILL.md")) == "" {
					t.Fatal("one batch's skills went into different backup folders")
				}
				folders = append(folders, folder)
			}
		}
	}
	if len(folders) != 2 || folders[0] == folders[1] {
		t.Fatalf("two removals did not keep distinct whole batches: %v", folders)
	}
	if got := read(t, filepath.Join(folders[0], "pdf", "forms.md")); got != "forms one" {
		t.Fatalf("the second removal changed the first backup: %q", got)
	}
}
