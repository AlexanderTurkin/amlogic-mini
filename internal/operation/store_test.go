package operation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReportMustBeOutsideSources(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	outside := filepath.Join(root, "source-other")
	for _, dir := range []string{source, outside} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := reportLocation(filepath.Join(source, "operation.json"), source); err == nil {
		t.Fatal("report accepted inside source")
	}
	if _, _, err := reportLocation(filepath.Join(outside, "operation.json"), source); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsReplacedStorage(t *testing.T) {
	for _, kind := range []string{"directory-before-write", "directory-after-write", "report"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "reports")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "operation.json")
			store, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "directory-before-write" {
				if err := store.Write("original", true); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "report" {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(dir, dir+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			replacement := path
			if kind == "directory-before-write" {
				replacement = filepath.Join(dir, "untouched")
			}
			if err := os.WriteFile(replacement, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := store.Write("next", kind == "directory-before-write"); err == nil {
				t.Fatal("replaced storage accepted")
			}
			data, err := os.ReadFile(replacement)
			if err != nil || string(data) != "replacement" {
				t.Fatalf("replacement changed: data=%q err=%v", data, err)
			}
		})
	}
}
