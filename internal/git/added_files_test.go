package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// AddedFiles (TASK-000167) took each added file's size from `ls-tree -l -r`
// of the whole tree. Its output grows with the repository, not with the
// attempt: past the capture limit (4 MiB, about 60 times Axiom-Office's tree)
// it is truncated, AddedFiles fails, and task.files_added is silently skipped
// with only a warning in the log — on exactly the large repositories where a
// stray file is hardest to spot. The sizes it needs are those of the few
// paths the attempt added, so that is all it should ask git about.

func TestAddedFilesDoesNotDependOnTheSizeOfTheRepository(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)

	// A tree whose full listing is far larger than the capture limit set
	// below, while the attempt adds two small files.
	for i := 0; i < 200; i++ {
		write(t, filepath.Join(repoPath, fmt.Sprintf("a-rather-long-directory-name-%03d.txt", i)), "x\n")
	}
	gitIn(t, repoPath, "add", "-A")
	gitIn(t, repoPath, "commit", "-qm", "many files")

	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000001-a1", Branch: "aidev/TASK-000001"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(wt.Path, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "data", "research.db"), "SQLite format 3\x00\x10\x00")
	write(t, filepath.Join(wt.Path, "notes.txt"), "hello\n")

	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatalf("CurrentTree: %v", err)
	}

	m.MaxOutputBytes = 2048 // the full ls-tree -l -r of this tree is several times this
	defer func() { m.MaxOutputBytes = DefaultMaxOutputBytes }()

	added, err := wt.AddedFiles(ctx, wt.BaseCommit, tree)
	if err != nil {
		t.Fatalf("AddedFiles: %v (the answer must not depend on the size of the whole tree)", err)
	}
	got := map[string]AddedFile{}
	for _, f := range added {
		got[f.Path] = f
	}
	if len(got) != 2 {
		t.Fatalf("AddedFiles = %+v, want data/research.db and notes.txt", added)
	}
	if f := got["data/research.db"]; f.Bytes != 18 || !f.Binary {
		t.Errorf("data/research.db = %+v, want 18 bytes, binary", f)
	}
	if f := got["notes.txt"]; f.Bytes != 6 || f.Binary {
		t.Errorf("notes.txt = %+v, want 6 bytes, not binary", f)
	}
}

// A path with characters a pathspec would read as magic is still sized.
func TestAddedFilesSizesPathsWithPathspecCharacters(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000002-a1", Branch: "aidev/TASK-000002"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	write(t, filepath.Join(wt.Path, ":(top)odd*name?.txt"), "abc\n")
	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	added, err := wt.AddedFiles(ctx, wt.BaseCommit, tree)
	if err != nil {
		t.Fatalf("AddedFiles: %v", err)
	}
	if len(added) != 1 || added[0].Path != ":(top)odd*name?.txt" || added[0].Bytes != 4 {
		t.Errorf("AddedFiles = %+v, want the odd name with 4 bytes", added)
	}
}
