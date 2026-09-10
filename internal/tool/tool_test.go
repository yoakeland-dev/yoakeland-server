package tool_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/tool"
)

func TestFileReadTool(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "sample.txt")
	_ = os.WriteFile(filePath, []byte("yoake test payload"), 0644)

	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	fileTool := tool.NewFileReadTool(norm)
	out, err := fileTool.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"path":"sample.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "yoake test payload" {
		t.Fatalf("unexpected content %q", out)
	}
}

func TestBashTool(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandbox.NewLocalSandbox(tempDir, 5*time.Second)
	bash := tool.NewBashTool(norm, sb)

	out, err := bash.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"command":"echo 'hello sandbox'"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello sandbox\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}
