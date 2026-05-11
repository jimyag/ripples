package git

import "testing"

func TestParseDiffIncludesDeletionOnlyChangedLines(t *testing.T) {
	diff := []byte(`diff --git a/service.go b/service.go
index 1111111..2222222 100644
--- a/service.go
+++ b/service.go
@@ -2,7 +2,6 @@ package main
 func run() {
 	start()
-	stop()
 	done()
 }
`)

	fileDiffs, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff() error = %v", err)
	}
	if len(fileDiffs) != 1 {
		t.Fatalf("len(fileDiffs) = %d, want 1", len(fileDiffs))
	}

	got := fileDiffs[0].ChangedLines
	if len(got) != 1 || got[0] != 4 {
		t.Fatalf("ChangedLines = %v, want [4]", got)
	}
}

func TestParseDiffKeepsDeletedGoFile(t *testing.T) {
	diff := []byte(`diff --git a/internal/old/old.go b/internal/old/old.go
deleted file mode 100644
index 1111111..0000000
--- a/internal/old/old.go
+++ /dev/null
@@ -1,5 +0,0 @@
-package old
-
-func Run() {
-}
`)

	fileDiffs, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff() error = %v", err)
	}
	if len(fileDiffs) != 1 {
		t.Fatalf("len(fileDiffs) = %d, want 1", len(fileDiffs))
	}
	if fileDiffs[0].Filename != "internal/old/old.go" {
		t.Fatalf("Filename = %q, want %q", fileDiffs[0].Filename, "internal/old/old.go")
	}
	if !fileDiffs[0].IsDeletedFile {
		t.Fatal("IsDeletedFile = false, want true")
	}
}

func TestParseDiffSkipsCommentOnlyLines(t *testing.T) {
	diff := []byte(`diff --git a/entity.go b/entity.go
index 1111111..2222222 100644
--- a/entity.go
+++ b/entity.go
@@ -2,7 +2,7 @@ package main
 type Instance struct {
-	// NodeID 节点 ID
+	// NodeID 节点名字
 	NodeID string
 }
`)

	fileDiffs, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff() error = %v", err)
	}
	if len(fileDiffs) != 1 {
		t.Fatalf("len(fileDiffs) = %d, want 1", len(fileDiffs))
	}
	if len(fileDiffs[0].ChangedLines) != 0 {
		t.Fatalf("ChangedLines = %v, want empty", fileDiffs[0].ChangedLines)
	}
}
