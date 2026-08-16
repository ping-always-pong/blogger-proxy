// =============================================================================
// 静态资源缓存模块
// =============================================================================
//
// 功能概述：
//   1. 缓存请求处理 — 接收 /static-cache/?url=... 请求，从缓存读取或下载资源
//   2. 并发控制 — 使用 singleflight 模式，相同 URL 的并发请求只下载一次
//   3. 原子写入 — 先写临时文件再重命名，防止写入中断导致缓存损坏
//   4. 自动清理 — 定期清理过期文件和超出容量限制的旧文件
//
//   5. CSS 重写 — 对于 CSS 文件（text/css），下载后重写其中的 url() 引用，
//      确保字体/图片等被引用的资源也通过代理加载
//
// 缓存文件命名规则：
//   SHA256(原始URL) + 清理后的扩展名
//   例如：a3f2b1c9... .css
//
// 并发安全设计：
//   ┌─────────────┐
//   │ 请求 1      │──── 文件不存在 ────┐
//   │ 请求 2      │──── 文件不存在 ────┤ singleflight.Do ──→ downloadAndCache
//   │ 请求 3      │──── 文件不存在 ────┘       │               (只执行一次)
//   └─────────────┘                             │
//       共享者（shared=true）←── 从缓存文件服务 ←┘
// =============================================================================

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/sync/singleflight"
)

// downloadGroup 是 singleflight 组，确保同一 URL 的并发下载请求只执行一次。
// singleflight 的工作原理：
//   - 第一个调用 Do(key, fn) 的协程会执行 fn
//   - 后续使用相同 key 调用的协程会阻塞等待，直到 fn 完成
//   - fn 完成后，所有协程都收到相同的结果
var downloadGroup singleflight.Group

// sharedHTTPClient 是复用的 HTTP 客户端，用于缓存资源下载。
// 复用客户端可以利用 HTTP 连接池，减少 TCP 握手开销。
var sharedHTTPClient = &http.Client{
	Timeout: 30 * time.Second, // 单次下载超时 30 秒
	Transport: &http.Transport{
		MaxIdleConns:        100,               // 最大空闲连接数
		IdleConnTimeout:     90 * time.Second,  // 空闲连接存活时间
		DisableCompression:  true,              // 禁用自动解压，直接存储原始字节
	},
}

// HandleCacheRequest 处理 /static-cache/?url=... 请求。
//
// 处理流程：
//   1. 从查询参数中提取 base64 编码的原始 URL
//   2. 计算 SHA256 哈希作为缓存文件名
//   3. 检查缓存文件是否存在 → 存在则直接服务（命中）
//   4. 不存在 → 使用 singleflight 下载（防止并发重复下载）
//   5. 等待者（shared=true） → 从缓存文件服务
//
// 参数：
//   w            — HTTP 响应写入器
//   r            — HTTP 请求（包含 ?url=... 查询参数）
//   cacheDir     — 缓存目录路径
//   cacheDomains — 缓存域名列表（用于 CSS url() 重写）
func HandleCacheRequest(w http.ResponseWriter, r *http.Request, cacheDir string, cacheDomains []string) {
	// ---- 第 1 步：解析 base64 编码的原始 URL ----
	encodedURL := r.URL.Query().Get("url")
	if encodedURL == "" {
		http.Error(w, "Missing url parameter", http.StatusBadRequest)
		return
	}

	decodedBytes, err := base64.URLEncoding.DecodeString(encodedURL)
	if err != nil {
		http.Error(w, "Invalid base64 URL", http.StatusBadRequest)
		return
	}

	targetURL := string(decodedBytes)

	// ---- 第 2 步：验证 URL 合法性 ----
	parsedURL, err := url.Parse(targetURL)
	if err != nil || !strings.HasPrefix(targetURL, "http") {
		http.Error(w, "Invalid target URL", http.StatusBadRequest)
		return
	}

	// ---- 第 3 步：计算缓存文件名 ----
	// 使用 SHA256 哈希确保文件名唯一且安全（无特殊字符）
	hash := sha256.Sum256([]byte(targetURL))
	hashStr := hex.EncodeToString(hash[:])

	// 从 URL 路径中提取扩展名，并做安全过滤
	// 只保留字母、数字和点号，过滤掉路径分隔符等危险字符
	ext := filepath.Ext(parsedURL.Path)
	if ext != "" {
		ext = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' {
				return r
			}
			return -1 // 返回 -1 表示丢弃该字符
		}, ext)
	}
	if ext == "" {
		// 无扩展名时，尝试从路径 basename 推断类型
		// 例如 Google Fonts URL: /css?family=Roboto → .css
		base := filepath.Base(parsedURL.Path)
		switch strings.ToLower(base) {
		case "css":
			ext = ".css"
		case "js":
			ext = ".js"
		default:
			ext = ".bin" // 无法识别扩展名时使用通用二进制扩展名
		}
	}

	filename := filepath.Join(cacheDir, hashStr+ext)

	// ---- 第 4 步：缓存命中检查 ----
	if _, err := os.Stat(filename); err == nil {
		// 文件已存在，直接从缓存服务
		serveFileWithHeaders(w, r, filename, targetURL)
		return
	}

	// ---- 第 5 步：缓存未命中，使用 singleflight 下载 ----
	// 关键：使用 singleflight 确保同一 URL 的并发请求只下载一次。
	// Do 的第三个返回值 shared 表示此调用者是否是"共享者"（等待者）。
	// shared=false → 是执行下载的那个协程
	// shared=true  → 是等待下载完成的协程
	_, err, shared := downloadGroup.Do(targetURL, func() (interface{}, error) {
		// 双重检查：获取锁后再次确认文件是否已被其他协程下载
		if _, err := os.Stat(filename); err == nil {
			return nil, nil // 已缓存，无需下载
		}
		// 执行下载（只有第一个协程会执行到这里）
		err := downloadAndCache(w, r, targetURL, filename, cacheDomains)
		return nil, err
	})

	// 下载失败：所有协程（执行者和等待者）都收到错误
	if err != nil {
		log.Printf("❌ Failed to download %s: %v", targetURL, err)
		http.Error(w, "Failed to download upstream resource", http.StatusBadGateway)
		return
	}

	// 下载成功：等待者从缓存文件服务
	// 执行者（shared=false）已经由 downloadAndCache 流式写入了响应，无需再处理
	if shared {
		if _, err := os.Stat(filename); err == nil {
			serveFileWithHeaders(w, r, filename, targetURL)
		} else {
			// 理论上不应出现：下载声称成功但文件不存在
			log.Printf("❌ Cache file missing after download for %s", targetURL)
			http.Error(w, "Failed to cache resource", http.StatusInternalServerError)
		}
	}
}

// downloadAndCache 从上游下载资源，同时写入缓存文件和 HTTP 响应。
//
// CSS 文件特殊处理：
//   对于 Content-Type 为 text/css 的响应，会先读取全部内容，重写其中的 url()
//   引用（将缓存域名替换为本地代理路径），再写入缓存和响应。这样 CSS 中引用的
//   字体、背景图片等资源也会通过代理加载，避免直接访问 Google 服务器。
//
// 其他类型文件（图片、JS 等）使用流式写入，避免内存占用。
//
// 原子写入机制：
//   1. 先写入临时文件（filename.tmp）
//   2. 写入完成后，使用 os.Rename 原子性地重命名为正式文件名
//   3. 如果中途失败，defer 会清理临时文件
//
// 优点：
//   - 不会出现"正在写入的半截文件"被其他请求读取
//   - os.Rename 在同一文件系统上是原子操作
//
// 参数：
//   w            — HTTP 响应写入器
//   r            — HTTP 请求（保留参数以备用）
//   targetURL    — 上游资源 URL
//   filename     — 缓存文件完整路径
//   cacheDomains — 缓存域名列表（用于 CSS url() 重写，非 CSS 文件忽略）
//
// 返回值：
//   error — 下载或写入失败时返回错误
func downloadAndCache(w http.ResponseWriter, r *http.Request, targetURL, filename string, cacheDomains []string) error {
	// ---- 从上游下载资源 ----
	// 使用原请求的 User-Agent，避免源站识别为机器人
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	// 复制客户端的 User-Agent
	if ua := r.Header.Get("User-Agent"); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	// 复制其他常见头，让请求更像真实浏览器
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	if acceptLang := r.Header.Get("Accept-Language"); acceptLang != "" {
		req.Header.Set("Accept-Language", acceptLang)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// ---- 确保缓存目录存在 ----
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	// ---- 创建临时文件 ----
	// 临时文件放在最终文件相同目录下，确保 Rename 是原子操作（同一文件系统）
	tmpFile := filename + ".tmp"
	// defer 保证无论函数如何退出，临时文件都会被清理
	defer func() {
		if _, err := os.Stat(tmpFile); err == nil {
			os.Remove(tmpFile)
		}
	}()

	contentType := resp.Header.Get("Content-Type")

	// ---- CSS 文件：先读取原始内容，检查编码，再重写 url() 引用 ----
	// CSS 文件通常较小（几 KB 到几百 KB），全部加载到内存不会造成问题。
	// 重写 url() 引用可以确保 CSS 中的字体/图片等资源也通过代理加载。
	//
	// 编码处理策略（与 HTML 处理保持一致）：
	//   先读取原始内容，检查是否为合法 UTF-8。如果是则直接使用，
	//   避免 charset.NewReader 在无法确定编码时默认使用 Windows-1252
	//   而导致 Unicode 字符（如 emoji）被错误转换。
	//   对于非 UTF-8 编码的 CSS，才使用 charset.NewReader 进行检测和转换。
	if strings.Contains(contentType, "text/css") {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read CSS body: %w", err)
		}

		if !utf8.Valid(bodyBytes) {
			// 内容不是合法 UTF-8，尝试 charset 检测和转换
			utf8Reader, err := charset.NewReader(bytes.NewReader(bodyBytes), contentType)
			if err != nil {
				// 编码检测失败，降级使用原始内容
				log.Printf("⚠ CSS charset detection failed for %s: %v, using original encoding", targetURL, err)
			} else {
				convertedBytes, err := io.ReadAll(utf8Reader)
				if err != nil {
					return fmt.Errorf("read converted CSS: %w", err)
				}
				bodyBytes = convertedBytes
			}
		}

		// 重写 CSS 中的 url() 引用
		rewritten := rewriteCSSUrls(string(bodyBytes), cacheDomains, nil)
		rewrittenBytes := []byte(rewritten)

		// 写入临时缓存文件
		if err := os.WriteFile(tmpFile, rewrittenBytes, 0644); err != nil {
			return fmt.Errorf("write CSS cache file: %w", err)
		}

		// 原子重命名：临时文件 → 正式文件
		if err := os.Rename(tmpFile, filename); err != nil {
			return fmt.Errorf("rename CSS cache file: %w", err)
		}

		// 复制上游响应头（Content-Type 等），但移除 Content-Encoding
		// 同时更新 Content-Length 为修改后的实际长度
		// 内容已转换为 UTF-8，更新 Content-Type 确保浏览器正确渲染
		copyHeaders(w.Header(), resp.Header)
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(rewrittenBytes)))

		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(rewrittenBytes); err != nil {
			return fmt.Errorf("write CSS response: %w", err)
		}

		log.Printf("✅ Cached and rewritten CSS: %s", targetURL)
		return nil
	}

	// ---- 非 CSS 文件：流式写入（图片、JS、字体等） ----
	bodyReader := resp.Body

	// 处理 gzip 压缩（与 modifyResponse 兜底逻辑一致）
	// 虽然 sharedHTTPClient 禁用了压缩，但某些 CDN 可能忽略并返回压缩内容
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(bodyReader)
		if err != nil {
			return fmt.Errorf("decompress gzip: %w", err)
		}
		defer gzReader.Close()
		bodyReader = gzReader
	}

	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	// 复制上游响应头（Content-Type 等），但移除 Content-Encoding
	// 因为我们写入的是解压后的原始字节
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Encoding")

	w.WriteHeader(resp.StatusCode)

	// 使用 TeeReader 同时写入缓存文件和 HTTP 响应
	// TeeReader 像一个 T 型三通：从 bodyReader 读取的数据会同时写入 f 和 w
	tee := io.TeeReader(bodyReader, f)
	if _, err := io.Copy(w, tee); err != nil {
		f.Close()
		return fmt.Errorf("stream response: %w", err)
	}

	f.Close()

	// ---- 原子重命名：临时文件 → 正式文件 ----
	if err := os.Rename(tmpFile, filename); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	log.Printf("✅ Cached resource: %s", targetURL)
	return nil
}

// serveFileWithHeaders 从本地缓存文件服务 HTTP 响应。
//
// 设置短缓存头（max-age=60），与 1 分钟 TTL 保持一致，
// 确保 URL 不变但内容变化时浏览器能及时重新请求。
//
// 参数：
//   w         — HTTP 响应写入器
//   r         — HTTP 请求
//   filename  — 缓存文件路径
//   originURL — 原始上游 URL（仅用于日志）
func serveFileWithHeaders(w http.ResponseWriter, r *http.Request, filename string, originURL string) {
	// 设置短缓存：缓存 TTL 默认仅 1 分钟，浏览器缓存时间应与之一致，
	// 确保 URL 不变但内容变化时能及时拉取新版本
	w.Header().Set("Cache-Control", "public, max-age=60")
	// http.ServeFile 会自动设置 Content-Type（根据扩展名）和 Content-Length
	http.ServeFile(w, r, filename)
	log.Printf("⚡ Served from cache: %s", originURL)
}

// copyHeaders 复制 HTTP 响应头。
// 使用 Add 而非 Set，保留同名的多个值（如 Set-Cookie）。
//
// 参数：
//   dst — 目标 Header
//   src — 源 Header
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// =============================================================================
// 缓存清理模块
// =============================================================================

// StartCacheCleanup 启动缓存清理任务。
//
// 执行时机：
//   1. 启动时立即执行一次清理
//   2. 之后每隔 1 分钟自动执行一次（与默认 TTL 1 分钟保持一致）
//
// 清理内容：
//   - 超过 TTL 的过期文件
//   - 残留的临时文件（.tmp 结尾且超过 TTL）
//   - 超出容量限制时，按时间从旧到新淘汰文件
//
// 参数：
//   cfg — 配置对象（包含缓存目录、TTL、容量上限等）
func StartCacheCleanup(cfg *Config) {
	cacheDir := cfg.Server.CacheDir

	// 解析 TTL 配置，无效时回退到默认 1 天
	ttl, err := time.ParseDuration(cfg.Server.CacheTTL)
	if err != nil {
		log.Printf("⚠ Invalid cache_ttl %q, using default 24h: %v", cfg.Server.CacheTTL, err)
		ttl = 24 * time.Hour
	}

	// 解析容量上限配置，无效时回退到默认 1GB
	maxSize, err := parseSize(cfg.Server.MaxCacheSize)
	if err != nil {
		log.Printf("⚠ Invalid max_cache_size %q, using default 1GB: %v", cfg.Server.MaxCacheSize, err)
		maxSize = 1 << 30 // 1GB = 1073741824 字节
	}

	// 启动时立即清理一次
	cleanCache(cacheDir, ttl, maxSize)

	// 启动后台定时清理协程
	// 清理间隔设为 TTL 的 1/24，最小 1 分钟，最大 1 小时
	// 这样既能及时清理过期文件，又不会在长 TTL 时频繁扫描
	cleanupInterval := ttl / 24
	if cleanupInterval < time.Minute {
		cleanupInterval = time.Minute
	}
	if cleanupInterval > time.Hour {
		cleanupInterval = time.Hour
	}
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			cleanCache(cacheDir, ttl, maxSize)
		}
	}()
}

// cleanCache 执行一次缓存清理。
//
// 清理策略：
//   1. 删除超过 TTL 的过期缓存文件
//   2. 删除残留的临时文件（.tmp 结尾且超过 TTL）
//   3. 如果总容量超过上限，按修改时间从旧到新淘汰
//
// 注意：过期文件在 cleanCache 中直接删除，而不是在 HandleCacheRequest 中判断，
// 这样避免了每次请求都检查文件时间，减少了磁盘 I/O。
//
// 参数：
//   cacheDir — 缓存目录路径
//   ttl      — 文件存活时间（超过即删除）
//   maxSize  — 最大容量（字节），超出后淘汰旧文件
func cleanCache(cacheDir string, ttl time.Duration, maxSize int64) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		log.Printf("⚠ Failed to read cache directory: %v", err)
		return
	}

	// 用于排序和统计的临时结构
	type fileInfo struct {
		path    string
		size    int64
		modTime time.Time
	}

	var files []fileInfo
	var totalSize int64
	cutoff := time.Now().Add(-ttl) // 过期时间点：现在 - TTL

	for _, entry := range entries {
		// 跳过子目录（当前设计不会产生子目录，但做防御性判断）
		if entry.IsDir() {
			continue
		}

		// 处理临时文件：下载中断或进程崩溃留下的 .tmp 文件
		if strings.HasSuffix(entry.Name(), ".tmp") {
			tmpPath := filepath.Join(cacheDir, entry.Name())
			info, _ := entry.Info()
			if info != nil && info.ModTime().Before(cutoff) {
				os.Remove(tmpPath)
				log.Printf("🧹 Removed stale temp file: %s", entry.Name())
			}
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		path := filepath.Join(cacheDir, entry.Name())

		// ---- 清理过期文件 ----
		// 文件修改时间早于 cutoff → 已过期，删除
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				log.Printf("🧹 Removed expired cache: %s", entry.Name())
			}
			continue
		}

		// 文件未过期，加入统计列表
		files = append(files, fileInfo{path: path, size: info.Size(), modTime: info.ModTime()})
		totalSize += info.Size()
	}

	// ---- 容量淘汰：超出限制时，从最旧的文件开始删除 ----
	evicted := 0
	if totalSize > maxSize && len(files) > 0 {
		// 按修改时间升序排列（最旧的在前）
		sort.Slice(files, func(i, j int) bool {
			return files[i].modTime.Before(files[j].modTime)
		})

		for _, f := range files {
			if totalSize <= maxSize {
				break
			}
			if err := os.Remove(f.path); err == nil {
				totalSize -= f.size
				evicted++
				log.Printf("🧹 Evicted cache (size limit): %s", filepath.Base(f.path))
			}
		}
		if evicted > 0 {
			log.Printf("📊 Evicted %d files to meet size limit", evicted)
		}
	}

	// 输出统计信息（文件数扣除了被淘汰的）
	log.Printf("📊 Cache stats: %d files, %.1f MB used",
		len(files)-evicted, float64(totalSize)/(1<<20))
}

// parseSize 将人类可读的容量字符串解析为字节数。
//
// 支持格式（不区分大小写）：
//   1TB  = 1099511627776 字节
//   1GB  = 1073741824 字节
//   500MB = 524288000 字节
//   100KB = 102400 字节
//   2048  = 2048 字节（纯数字，视为字节）
//   1.5GB = 1610612736 字节（支持小数）
//
// 参数：
//   s — 容量字符串
//
// 返回值：
//   int64 — 字节数
//   error — 无法解析时返回错误
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// 按后缀长度降序排列，确保 "KB" 在 "B" 之前匹配
	// （否则 "100KB" 会被 "B" 后缀先匹配到）
	multipliers := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1 << 40}, // 1024^4
		{"GB", 1 << 30}, // 1024^3
		{"MB", 1 << 20}, // 1024^2
		{"KB", 1 << 10}, // 1024
		{"B", 1},        // 1
	}

	for _, m := range multipliers {
		if strings.HasSuffix(s, m.suffix) {
			// 提取数字部分：字符串去掉后缀
			numStr := strings.TrimSuffix(s, m.suffix)
			numStr = strings.TrimSpace(numStr)
			var val float64
			// 使用 float64 解析支持小数（如 1.5GB）
			if _, err := fmt.Sscanf(numStr, "%f", &val); err != nil {
				return 0, err
			}
			return int64(val * float64(m.mult)), nil
		}
	}

	// 没有匹配到后缀，尝试作为纯数字（字节）解析
	var val int64
	if _, err := fmt.Sscanf(s, "%d", &val); err != nil {
		return 0, err
	}
	return val, nil
}