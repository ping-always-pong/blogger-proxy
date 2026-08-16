// =============================================================================
// 缓存模块测试
// =============================================================================
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseSize 验证容量字符串解析功能的正确性。
// 覆盖各种人类可读格式：GB、MB、KB、TB、纯数字、小数、无效输入。
func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string // 输入字符串
		expected int64  // 期望的字节数
		wantErr  bool   // 是否期望返回错误
	}{
		{"1GB", 1 << 30, false},                           // 1GB = 1073741824 字节
		{"500MB", 500 << 20, false},                       // 500MB = 524288000 字节
		{"100KB", 100 << 10, false},                       // 100KB = 102400 字节
		{"1TB", 1 << 40, false},                           // 1TB = 1099511627776 字节
		{"2048", 2048, false},                             // 纯数字视为字节
		{"1.5GB", int64(1.5 * (1 << 30)), false},          // 支持小数
		{"", 0, true},                                     // 空字符串应报错
		{"invalid", 0, true},                              // 无法解析的字符串应报错
	}

	for _, tt := range tests {
		got, err := parseSize(tt.input)
		if tt.wantErr && err == nil {
			t.Errorf("parseSize(%q) expected error, got %d", tt.input, got)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", tt.input, err)
		}
		if !tt.wantErr && got != tt.expected {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

// TestCleanCache_ExpiredFiles 验证：超过 TTL 的过期文件被正确清理。
// 同时验证：未过期的文件保留，残留的 .tmp 文件被清理。
func TestCleanCache_ExpiredFiles(t *testing.T) {
	dir := t.TempDir()

	// 创建一个"旧"文件（200小时前），应被清理
	oldFile := filepath.Join(dir, "old.css")
	if err := os.WriteFile(oldFile, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-200 * time.Hour) // 早于 168h 的 TTL
	os.Chtimes(oldFile, oldTime, oldTime)

	// 创建一个"新"文件（当前时间），应保留
	recentFile := filepath.Join(dir, "recent.js")
	if err := os.WriteFile(recentFile, []byte("recent"), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建一个过期的临时文件，应被清理
	tmpFile := filepath.Join(dir, "stale.tmp")
	if err := os.WriteFile(tmpFile, []byte("tmp"), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(tmpFile, oldTime, oldTime)

	// 执行清理：TTL = 168h（7天），容量上限 = 1GB（足够大，不会触发淘汰）
	cleanCache(dir, 168*time.Hour, 1<<30)

	// 验证：旧文件被删除
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("expired file should have been removed")
	}
	// 验证：新文件保留
	if _, err := os.Stat(recentFile); os.IsNotExist(err) {
		t.Error("recent file should not have been removed")
	}
	// 验证：临时文件被清理
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Error("stale temp file should have been removed")
	}
}

// TestCleanCache_SizeLimit 验证：超出容量限制时，按时间从旧到新淘汰文件。
// 场景：3 个文件各 100 字节，总 300 字节，限制 150 字节。
// 期望：最旧的 2 个文件被淘汰，最新的 1 个保留。
func TestCleanCache_SizeLimit(t *testing.T) {
	dir := t.TempDir()

	// 创建 3 个文件，每个 100 字节，修改时间错开
	for i, name := range []string{"a.css", "b.js", "c.png"} {
		path := filepath.Join(dir, name)
		content := make([]byte, 100)
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatal(err)
		}
		// a.css 最旧（3小时前），c.png 最新（1小时前）
		mt := time.Now().Add(-time.Duration(3-i) * time.Hour)
		os.Chtimes(path, mt, mt)
	}

	// 限制 150 字节：300 → 删除 a.css → 200 → 删除 b.js → 100（低于 150，停止）
	cleanCache(dir, 168*time.Hour, 150)

	// 验证：最旧的两个文件被删除
	if _, err := os.Stat(filepath.Join(dir, "a.css")); !os.IsNotExist(err) {
		t.Error("a.css (oldest) should have been evicted")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.js")); !os.IsNotExist(err) {
		t.Error("b.js (second oldest) should have been evicted")
	}
	// 验证：最新的文件保留
	if _, err := os.Stat(filepath.Join(dir, "c.png")); os.IsNotExist(err) {
		t.Error("c.png (newest) should not have been evicted")
	}
}

// TestCleanCache_EmptyDir 验证：空目录清理不会崩溃。
func TestCleanCache_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cleanCache(dir, 168*time.Hour, 1<<30)
}

// TestCleanCache_NonexistentDir 验证：不存在目录清理不会崩溃（应优雅处理）。
func TestCleanCache_NonexistentDir(t *testing.T) {
	cleanCache("/nonexistent/path", 168*time.Hour, 1<<30)
}