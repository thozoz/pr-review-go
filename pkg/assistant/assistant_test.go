package assistant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecurity_WorkspaceSiblingAccessDenied(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	siblingDir := filepath.Join(baseDir, "workspace-sibling")

	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workDir: %v", err)
	}
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatalf("failed to create siblingDir: %v", err)
	}

	secretFile := filepath.Join(siblingDir, "secret")
	secretContent := "super-secret-sibling-data"
	if err := os.WriteFile(secretFile, []byte(secretContent), 0644); err != nil {
		t.Fatalf("failed to write secret file: %v", err)
	}

	a := &Assistant{}
	ctx := context.Background()

	// 1. Read sibling secret via relative path traversal
	readResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "read_file",
		Path:   "../workspace-sibling/secret",
	})
	if strings.Contains(readResp, secretContent) {
		t.Fatalf("VULNERABILITY: sibling secret was read: %s", readResp)
	}
	if !strings.Contains(readResp, "Access denied") && !strings.Contains(readResp, "access denied") {
		t.Errorf("expected access denied for sibling read, got: %s", readResp)
	}

	// 2. Write sibling file via relative path traversal
	writeResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "write_file",
		Path:   "../workspace-sibling/secret",
	})
	data, _ := os.ReadFile(secretFile)
	if string(data) == "overwritten-data" {
		t.Fatalf("VULNERABILITY: sibling secret was overwritten: %s", writeResp)
	}
	if !strings.Contains(strings.ToLower(writeResp), "disabled") && !strings.Contains(strings.ToLower(writeResp), "access denied") {
		t.Errorf("expected access denied or disabled for sibling write, got: %s", writeResp)
	}
}

func TestSecurity_SymlinkOutDenied(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	outsideDir := filepath.Join(baseDir, "outside")

	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workDir: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatalf("failed to create outsideDir: %v", err)
	}

	secretFile := filepath.Join(outsideDir, "secret.txt")
	secretContent := "outside-confidential-token"
	if err := os.WriteFile(secretFile, []byte(secretContent), 0644); err != nil {
		t.Fatalf("failed to write secret file: %v", err)
	}

	symlinkFile := filepath.Join(workDir, "symlink-to-secret.txt")
	if err := os.Symlink(secretFile, symlinkFile); err != nil {
		t.Skipf("skipping symlink test on platform without symlink support: %v", err)
	}

	a := &Assistant{}
	ctx := context.Background()

	// 1. Read file through symlink pointing outside workspace
	readResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "read_file",
		Path:   "symlink-to-secret.txt",
	})
	if strings.Contains(readResp, secretContent) {
		t.Fatalf("VULNERABILITY: read secret file through symlink pointing outside: %s", readResp)
	}
	if !strings.Contains(readResp, "Access denied") && !strings.Contains(readResp, "access denied") {
		t.Errorf("expected access denied for symlink read, got: %s", readResp)
	}

	// 2. Write file through symlink pointing outside workspace
	writeResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "write_file",
		Path:   "symlink-to-secret.txt",
	})
	data, _ := os.ReadFile(secretFile)
	if string(data) == "tampered-via-symlink" {
		t.Fatalf("VULNERABILITY: wrote to outside file through symlink: %s", writeResp)
	}
	if !strings.Contains(strings.ToLower(writeResp), "disabled") && !strings.Contains(strings.ToLower(writeResp), "access denied") {
		t.Errorf("expected access denied or disabled for symlink write, got: %s", writeResp)
	}

	// 3. Symlink directory pointing outside workspace, then write new file inside it
	symlinkDir := filepath.Join(workDir, "symlink-dir")
	if err := os.Symlink(outsideDir, symlinkDir); err == nil {
		nestedWriteResp := a.executeTool(ctx, workDir, &ToolCallRequest{
			Action: "write_file",
			Path:   "symlink-dir/new-outside-file.txt",
		})
		outsideNewFile := filepath.Join(outsideDir, "new-outside-file.txt")
		if _, err := os.Stat(outsideNewFile); err == nil {
			t.Fatalf("VULNERABILITY: created file outside workspace via symlink directory: %s", nestedWriteResp)
		}
		if !strings.Contains(strings.ToLower(nestedWriteResp), "disabled") && !strings.Contains(strings.ToLower(nestedWriteResp), "access denied") {
			t.Errorf("expected access denied or disabled for symlink-dir write, got: %s", nestedWriteResp)
		}
	}
}

func TestSecurity_GitDirectoryWriteDenied(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	gitHooksDir := filepath.Join(workDir, ".git", "hooks")
	if err := os.MkdirAll(gitHooksDir, 0755); err != nil {
		t.Fatalf("failed to create git hooks dir: %v", err)
	}

	a := &Assistant{}
	ctx := context.Background()

	// Write hook inside .git
	hookPath := filepath.Join(".git", "hooks", "pre-commit")
	writeResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "write_file",
		Path:   hookPath,
	})

	hookTarget := filepath.Join(workDir, ".git", "hooks", "pre-commit")
	if _, err := os.Stat(hookTarget); err == nil {
		t.Fatalf("VULNERABILITY: wrote to .git directory: %s", writeResp)
	}
	if !strings.Contains(strings.ToLower(writeResp), "disabled") && !strings.Contains(strings.ToLower(writeResp), "access denied") {
		t.Errorf("expected access denied or disabled for .git write, got: %s", writeResp)
	}
}

func TestSecurity_WriteFileRejectedWithoutModifyingFiles(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workDir: %v", err)
	}

	a := &Assistant{}
	ctx := context.Background()

	step, err := parseToolCall(`{"action":"write_file","path":"nested/deep/directory/test.txt","content":"should not be written"}`)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.executeTool(ctx, workDir, step)

	if !strings.Contains(strings.ToLower(resp), "disabled") && !strings.Contains(strings.ToLower(resp), "read-only") {
		t.Errorf("expected explicit informative error about disabled write_file/read-only, got: %s", resp)
	}

	nestedDir := filepath.Join(workDir, "nested")
	if _, err := os.Stat(nestedDir); err == nil {
		t.Fatalf("VULNERABILITY: write_file created nested directories despite being disabled")
	}
	targetFile := filepath.Join(workDir, "nested", "deep", "directory", "test.txt")
	if _, err := os.Stat(targetFile); err == nil {
		t.Fatalf("VULNERABILITY: write_file created file despite being disabled")
	}
}

func TestSecurity_RunCommandRejectedWithoutExecuting(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workDir: %v", err)
	}

	markerFile := filepath.Join(baseDir, "marker-never-execute.txt")
	_ = os.Remove(markerFile)

	a := &Assistant{}
	ctx := context.Background()

	// Legacy command requests must not execute, even when they include a command.
	raw, err := json.Marshal(map[string]string{"action": "run_command", "command": "echo injected > " + markerFile})
	if err != nil {
		t.Fatal(err)
	}
	step, err := parseToolCall(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp := a.executeTool(ctx, workDir, step)

	if _, err := os.Stat(markerFile); err == nil {
		t.Fatalf("VULNERABILITY: run_command executed and created marker file outside: %s", resp)
	}

	if !strings.Contains(strings.ToLower(resp), "disabled") && !strings.Contains(strings.ToLower(resp), "isolation") {
		t.Errorf("expected explicit informative error about disabled run_command/isolation, got: %s", resp)
	}
}

func TestSecurity_CommitAndPushRejectedBeforeGit(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workDir: %v", err)
	}

	a := &Assistant{}
	ctx := context.Background()

	step, err := parseToolCall(`{"action":"commit_and_push","commit_msg":"malicious commit"}`)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.executeTool(ctx, workDir, step)

	if strings.Contains(resp, "Error staging files") || strings.Contains(resp, "Git commit failed") || strings.Contains(resp, "Git push failed") {
		t.Fatalf("VULNERABILITY: commit_and_push invoked host git without container isolation: %s", resp)
	}

	if !strings.Contains(strings.ToLower(resp), "disabled") && !strings.Contains(strings.ToLower(resp), "isolation") {
		t.Errorf("expected explicit informative error about disabled commit_and_push/isolation, got: %s", resp)
	}
}

func TestAssistant_ValidWorkspaceOperations(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workDir: %v", err)
	}

	a := &Assistant{}
	ctx := context.Background()

	// 1. Attempt write_file inside workspace (must be rejected as read-only, no files or dirs created)
	writeResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "write_file",
		Path:   "nested/dir/hello.go",
	})
	if !strings.Contains(strings.ToLower(writeResp), "disabled") && !strings.Contains(strings.ToLower(writeResp), "read-only") {
		t.Fatalf("expected write_file to be rejected as read-only/disabled, got: %s", writeResp)
	}
	targetFile := filepath.Join(workDir, "nested", "dir", "hello.go")
	if _, err := os.Stat(targetFile); err == nil {
		t.Fatalf("expected file to not be written, but it exists")
	}
	nestedDir := filepath.Join(workDir, "nested")
	if _, err := os.Stat(nestedDir); err == nil {
		t.Fatalf("expected nested directory to not be created, but it exists")
	}

	// Prepare existing file in workspace to verify valid read and list operations
	if err := os.MkdirAll(filepath.Dir(targetFile), 0755); err != nil {
		t.Fatalf("failed to create target file directory: %v", err)
	}
	if err := os.WriteFile(targetFile, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// 2. Read file inside workspace
	readResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "read_file",
		Path:   "nested/dir/hello.go",
	})
	if readResp != "package main\n\nfunc main() {}\n" {
		t.Fatalf("expected file content, got: %s", readResp)
	}

	// 3. List files inside workspace
	listResp := a.executeTool(ctx, workDir, &ToolCallRequest{
		Action: "list_files",
		Path:   ".",
	})
	if !strings.Contains(listResp, "nested") || !strings.Contains(listResp, "hello.go") {
		t.Fatalf("expected list_files to include nested/dir/hello.go, got: %s", listResp)
	}
}
