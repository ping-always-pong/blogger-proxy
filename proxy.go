// =============================================================================
// 反向代理与 HTML 重写模块
// =============================================================================
//
// 核心功能：
//   1. 反向代理：将 HTTP 请求转发到上游 Blogger 托管服务
//   2. HTML 重写：拦截 HTML 响应，将其中匹配 cache_domains 的静态资源 URL
//      替换为本地缓存路径 /static-cache/?url=<base64编码的原始URL>
//   3. 域名白名单：只代理配置中允许的域名，拒绝其他请求
//
// 重写覆盖范围：
//   - <link href="...">         — 样式表（同站链接转为相对路径，外部链接走缓存）
//   - <script src="...">        — JavaScript（同站链接转为相对路径，外部链接走缓存）
//   - <img src="..." srcset="...">  — 图片（含响应式图片）
//   - <source src="..." srcset="..."> — 媒体资源源
//   - <video poster="..." src="...">  — 视频
//   - <audio src="...">         — 音频
//   - <iframe src="...">        — 内嵌框架
//   - <embed src="...">         — 嵌入内容
//   - <track src="...">         — 字幕/章节
//   - <input src="...">         — 图片按钮
//   - <object data="...">       — 嵌入对象
//   - <use href="...">          — SVG 引用
//   - <feImage href="...">      — SVG 滤镜图片
//   - style="..." 属性中的 url() — 内联样式
//   - <style> 标签内容中的 url() — 样式块
//   - data-src / data-srcset    — 懒加载图片属性
//   - <a href="...">            — 同站导航链接（去掉协议和域名，转为相对路径）
//
// 注意：
//   - 同站链接（<a href>、<link href>、<script src>、<img src> 等，以及 CSS url()）
//     会被重写为相对路径，避免 http/https 切换；外部链接仍按 cache_domains 缓存逻辑处理。
//   - 外部链接、锚点、mailto/javascript/tel/data 等特殊协议保持原样。
// =============================================================================

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// urlAttrs 定义了每种 HTML 标签中需要重写的 URL 属性名。
// 键是标签名（小写），值是需要处理的属性名列表。
// 新增需要缓存的资源类型时，只需在此 map 中添加对应的标签和属性即可。
var urlAttrs = map[string][]string{
	"link":    {"href"},                             // 外部样式表、favicon 等
	"script":  {"src"},                              // JavaScript 脚本
	"img":     {"src", "srcset", "data-src", "data-srcset"}, // 图片（含懒加载和响应式）
	"iframe":  {"src"},                              // 内嵌框架
	"source":  {"src", "srcset", "data-srcset"},     // <picture>/<video>/<audio> 的源
	"audio":   {"src"},                              // 音频
	"video":   {"src", "poster"},                    // 视频 + 封面图
	"embed":   {"src"},                              // 嵌入内容（如 Flash）
	"track":   {"src"},                              // 字幕/章节轨道
	"input":   {"src"},                              // type="image" 的图片按钮
	"object":  {"data"},                             // 嵌入对象
	"use":     {"href"},                             // SVG use 引用
	"feimage": {"href"},                             // SVG 滤镜图片
}

// cssURLRegex 匹配 CSS 中的 url() 引用。
// 支持三种格式：url(https://...)、url("https://...")、url('https://...')
// 以及内部有空格的情况：url( "https://..." )
var cssURLRegex = regexp.MustCompile(`url\(\s*["']?\s*([^)"'\s]+)\s*["']?\s*\)`)

// ProxyHandler 是代理处理器的核心结构体。
// 它实现了 http.Handler 接口，处理所有进入的 HTTP 请求。
type ProxyHandler struct {
	Config       *Config               // 配置对象
	ReverseProxy *httputil.ReverseProxy // Go 标准库反向代理
	CacheDomains []string               // 需要缓存的资源域名列表（从配置读取）
	Client       *http.Client           // 带超时配置的 HTTP 客户端
}

// NewProxyHandler 创建并初始化代理处理器。
//
// 处理流程：
//   1. 解析上游目标地址
//   2. 创建带超时配置的 HTTP 客户端（连接超时、TLS 握手超时等）
//   3. 配置反向代理的 Director（修改请求头）
//   4. 注册 ModifyResponse 回调（HTML 响应后处理）
//
// 参数：
//   cfg — 配置对象
//
// 返回值：
//   *ProxyHandler — 初始化完成的代理处理器
//   error         — 上游地址解析失败时返回错误
func NewProxyHandler(cfg *Config) (*ProxyHandler, error) {
	// 解析上游 Blogger 托管地址
	target, err := url.Parse(cfg.Server.TargetAddress)
	if err != nil {
		return nil, err
	}

	h := &ProxyHandler{
		Config:       cfg,
		CacheDomains: cfg.Server.CacheDomains,
	}

	// ---- 创建带超时配置的 HTTP 客户端 ----
	// 详细的超时配置防止上游无响应时连接泄漏和协程阻塞
	client := &http.Client{
		// 整体超时：整个请求的生命周期不超过 30 秒
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second, // TCP 连接超时
				KeepAlive: 30 * time.Second, // TCP Keep-Alive 探测间隔
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second, // TLS 握手超时
			ResponseHeaderTimeout: 30 * time.Second, // 等待响应头超时
			ExpectContinueTimeout: 1 * time.Second,  // Expect: 100-continue 超时
			MaxIdleConns:          100,               // 最大空闲连接数
			IdleConnTimeout:       90 * time.Second,  // 空闲连接存活时间
			DisableCompression:    true,               // 禁用自动解压（需要原始 HTML 进行重写）
		},
	}
	h.Client = client

	// ---- 创建单主机反向代理 ----
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = client.Transport // 复用我们配置的 Transport

	// 保存原始 Director，在其基础上添加自定义逻辑
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		// 1. 先保存原始 Host（originalDirector 会将其覆盖为目标地址的 Host）
		originalHost := req.Host

		// 2. 执行标准反向代理逻辑（重写 URL、设置 X-Forwarded-* 头等）
		originalDirector(req)

		// 3. 恢复原始 Host 头（Blogger 需要正确的 Host 来路由到对应博客）
		req.Host = originalHost

		// 4. 移除 Accept-Encoding 头，强制上游返回未压缩内容
		//    因为我们需要解析 HTML 来重写资源链接，压缩内容无法直接解析
		req.Header.Del("Accept-Encoding")
	}

	// 注册响应修改回调——在收到上游 HTML 响应后，重写其中的资源链接
	proxy.ModifyResponse = h.modifyResponse

	h.ReverseProxy = proxy
	return h, nil
}

// shouldCacheHost 检查给定的主机名是否匹配任一缓存域名。
//
// 支持两种匹配模式：
//   1. 精确匹配：host == "fonts.googleapis.com"
//   2. 子域名匹配：host == "abc.lh3.googleusercontent.com" 匹配 "lh3.googleusercontent.com"
//
// 参数：
//   host         — 从 URL 中解析出的主机名
//   cacheDomains — 缓存域名列表
//
// 返回值：
//   bool — 是否应该缓存该主机上的资源
func shouldCacheHost(host string, cacheDomains []string) bool {
	for _, d := range cacheDomains {
		// 精确匹配 或 以 ".域名" 结尾（子域名匹配）
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// rewriteURL 将原始资源 URL 转换为本地缓存代理 URL。
//
// 转换示例：
//   输入：https://fonts.googleapis.com/css?family=Roboto
//   输出：/static-cache/?url=aHR0cHM6Ly9mb250cy5nb29nbGVhcGlzLmNvbS9jc3M...
//
// 参数：
//   rawURL — 原始资源 URL（可能是协议相对 URL 如 //example.com/a.css）
//
// 返回值：
//   string — 本地缓存代理 URL
func rewriteURL(rawURL string) string {
	// 协议相对 URL 补全为 https:// 以便后续处理
	if strings.HasPrefix(rawURL, "//") {
		rawURL = "https:" + rawURL
	}
	// Base64 URL 编码（使用 URL 安全字符集，避免 +/= 等需要转义的字符）
	encoded := base64.URLEncoding.EncodeToString([]byte(rawURL))
	return "/static-cache/?url=" + encoded
}

// normalizeHost 规范化主机名：去除两端空白、转小写、去掉端口号。
// 用于同站链接比较时忽略大小写和端口差异。
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// hostMatches 检查 host 是否与 sameSiteHosts 中任一主机名匹配。
func hostMatches(host string, sameSiteHosts []string) bool {
	normalized := normalizeHost(host)
	for _, h := range sameSiteHosts {
		if normalizeHost(h) == normalized {
			return true
		}
	}
	return false
}

// buildRelativeURL 从已解析的绝对 URL 构造相对路径，保留 query 和 fragment。
func buildRelativeURL(u *url.URL) string {
	relative := u.EscapedPath()
	if relative == "" {
		relative = "/"
	}
	if u.RawQuery != "" {
		relative += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		relative += "#" + u.Fragment
	}
	return relative
}

// makeSameSiteRelative 将指向本站（或配置的同一站点别名）的绝对链接转换为相对路径。
//
// 会处理以下形式：
//   https://www.my-blog.com/path   -> /path
//   http://www.my-blog.com/path    -> /path
//   //www.my-blog.com/path         -> /path
//
// 以下情况保持原样：
//   - 已经是相对路径：/path、./path、../path
//   - 纯锚点：#section
//   - 非 http/https 协议：mailto:、javascript:、tel:、data: 等
//   - 指向外部域名的链接
func makeSameSiteRelative(rawURL string, sameSiteHosts []string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return rawURL
	}

	// 已经是相对路径或纯锚点：无需处理
	// 注意：先判断 // 协议相对 URL，再判断 / 开头，避免 //example.com 被误当作相对路径
	if strings.HasPrefix(rawURL, "//") {
		parsed, err := url.Parse("https:" + rawURL)
		if err != nil {
			return rawURL
		}
		if hostMatches(parsed.Host, sameSiteHosts) {
			return buildRelativeURL(parsed)
		}
		return rawURL
	}
	if strings.HasPrefix(rawURL, "#") ||
		strings.HasPrefix(rawURL, "/") ||
		strings.HasPrefix(rawURL, "./") ||
		strings.HasPrefix(rawURL, "../") {
		return rawURL
	}

	// 识别 scheme，排除 "path:file" 这类没有 scheme 的相对路径写法
	colonIdx := strings.Index(rawURL, ":")
	slashIdx := strings.Index(rawURL, "/")
	if colonIdx < 0 || (slashIdx >= 0 && colonIdx > slashIdx) {
		return rawURL
	}

	scheme := strings.ToLower(rawURL[:colonIdx])
	if scheme != "http" && scheme != "https" {
		return rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if hostMatches(parsed.Host, sameSiteHosts) {
		return buildRelativeURL(parsed)
	}
	return rawURL
}

// rewriteSrcset 重写 HTML srcset 属性中的所有资源 URL。
//
// srcset 格式：描述符之间用逗号分隔，每个条目是 "URL 描述符" 的格式。
// 例如：
//   输入：https://cdn.com/small.jpg 480w, https://cdn.com/large.jpg 1024w
//   输出：/static-cache/?url=... 480w, /static-cache/?url=... 1024w
//
// 参数：
//   srcset         — 原始 srcset 属性值
//   sameSiteHosts  — 视为同一站点的主机名列表
//
// 返回值：
//   string — 重写后的 srcset 属性值
func (h *ProxyHandler) rewriteSrcset(srcset string, sameSiteHosts []string) string {
	parts := strings.Split(srcset, ",")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		// 每个条目格式：URL [可选描述符]
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}

		// 先尝试同站相对化
		relative := makeSameSiteRelative(fields[0], sameSiteHosts)
		if relative != fields[0] {
			fields[0] = relative
			parts[i] = strings.Join(fields, " ")
			continue
		}

		// 解析 URL 获取主机名
		parsed, err := url.Parse(fields[0])
		if err != nil {
			continue
		}
		// 只有匹配缓存域名的 URL 才重写
		if shouldCacheHost(parsed.Host, h.CacheDomains) {
			fields[0] = rewriteURL(fields[0])
			parts[i] = strings.Join(fields, " ")
		}
	}
	return strings.Join(parts, ", ")
}

// rewriteCSSUrls 重写 CSS 文本中所有 url() 引用里的资源 URL。
//
// 处理三种 CSS url() 格式：
//   url(https://example.com/bg.jpg)
//   url("https://example.com/bg.jpg")
//   url('https://example.com/bg.jpg')
//
// 同站 URL 会先被转换为相对路径；非本站 URL 再判断是否在缓存域名列表中。
// 不会重写 data: URL（如 data:image/png;base64,...），因为 url.Parse 解析后
// 主机名为空，shouldCacheHost("") 返回 false。
//
// 参数：
//   css            — CSS 文本内容
//   cacheDomains   — 缓存域名列表
//   sameSiteHosts  — 视为同一站点的主机名列表
//
// 返回值：
//   string — 重写后的 CSS 文本
func rewriteCSSUrls(css string, cacheDomains []string, sameSiteHosts []string) string {
	return cssURLRegex.ReplaceAllStringFunc(css, func(match string) string {
		// 从 "url(https://...)" 中提取 URL 部分
		// match[4:] 跳过 "url(" 前缀，match[:len(match)-1] 去掉末尾 ")"
		inner := match[4 : len(match)-1]
		inner = strings.TrimSpace(inner)
		inner = strings.Trim(inner, `"'`) // 去掉可能的引号

		// 先尝试同站相对化
		relative := makeSameSiteRelative(inner, sameSiteHosts)
		if relative != inner {
			return "url(" + relative + ")"
		}

		parsed, err := url.Parse(inner)
		if err != nil {
			return match // 无法解析则保持原样
		}
		if shouldCacheHost(parsed.Host, cacheDomains) {
			newURL := rewriteURL(inner)
			return "url(" + newURL + ")"
		}
		return match // 不在缓存域名列表中，保持原样
	})
}

// modifyResponse 是 httputil.ReverseProxy 的 ModifyResponse 回调函数。
// 在上游返回 HTML 响应后自动调用，用于重写其中的资源链接。
//
// 处理流程：
//   1. 检查 Content-Type 是否为 text/html（非 HTML 响应直接跳过）
//   2. 处理 gzip 压缩（解压后才能解析 HTML）
//   3. 使用 HTML tokenizer 遍历 DOM，重写资源 URL
//   4. 更新 Content-Length 头为修改后的实际长度
//
// 参数：
//   resp — 上游返回的 HTTP 响应
//
// 返回值：
//   error — 处理失败时返回错误
func (h *ProxyHandler) modifyResponse(resp *http.Response) error {
	// 只处理 HTML 响应，其他类型（CSS/JS/图片等）不处理
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		return nil
	}

	bodyReader := resp.Body

	// ---- 处理 gzip 压缩 ----
	// 因为我们在请求中移除了 Accept-Encoding，大多数情况下上游不会压缩。
	// 但某些 CDN 可能忽略该头，所以这里做兜底处理。
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(bodyReader)
		if err != nil {
			return err
		}
		defer gzReader.Close()
		bodyReader = gzReader
		resp.Header.Del("Content-Encoding") // 解压后不再是 gzip 编码
	} else if resp.Header.Get("Content-Encoding") != "" {
		// 遇到未知编码格式，记录警告但不中断处理
		log.Printf("Warning: Unknown encoding %s, might not be able to parse HTML", resp.Header.Get("Content-Encoding"))
	}

	// ---- 读取 HTML body 并处理字符编码 ----
	// 先读取原始响应体，检查是否为合法的 UTF-8 编码。
	// 如果内容已经是合法 UTF-8（绝大多数 Blogger 博客都是 UTF-8），
	// 则直接使用，跳过 charset 检测。这可以避免 charset.NewReader
	// 在无法确定编码时默认使用 Windows-1252 而导致 emoji 等
	// 多字节 Unicode 字符（4 字节 UTF-8 序列）被错误转换的问题。
	//
	// 对于非 UTF-8 编码的页面（如 GBK、ISO-8859-1），
	// charset.NewReader 会自动检测并转换为 UTF-8，
	// 因为 html.NewTokenizer 只能正确处理 UTF-8 输入。
	bodyBytes, err := io.ReadAll(bodyReader)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if !utf8.Valid(bodyBytes) {
		// 内容不是合法 UTF-8，尝试 charset 检测和转换
		utf8Reader, err := charset.NewReader(bytes.NewReader(bodyBytes), contentType)
		if err != nil {
			// 编码检测失败时不中断处理，降级使用原始内容
			// （可能是纯 ASCII 内容或 charset 标签格式异常）
			log.Printf("⚠ Charset detection failed, using original encoding: %v", err)
		} else {
			convertedBytes, err := io.ReadAll(utf8Reader)
			if err != nil {
				return err
			}
			bodyBytes = convertedBytes
		}
	}

	// ---- 重写 HTML 中的资源链接 ----
	// 从原始请求中获取当前站点域名，并追加配置的同一站点别名
	siteHost := resp.Request.Host
	if siteHost == "" && resp.Request.URL != nil {
		siteHost = resp.Request.URL.Host
	}
	sameSiteHosts := []string{siteHost}
	if h.Config != nil {
		sameSiteHosts = append(sameSiteHosts, h.Config.Server.SameSiteDomains...)
	}
	modifiedBody := h.rewriteHTML(bodyBytes, sameSiteHosts)

	// ---- 更新响应 Body 和头信息 ----
	resp.Body = io.NopCloser(bytes.NewReader(modifiedBody))
	resp.ContentLength = int64(len(modifiedBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(modifiedBody)))
	// 内容已转换为 UTF-8，更新 Content-Type 头确保浏览器正确渲染
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")

	return nil
}

// rewriteHTML 使用 HTML tokenizer 遍历文档，重写其中的资源 URL。
//
// 为什么用 tokenizer 而不是正则表达式？
//   正则表达式无法区分 HTML 属性值中的 URL 和普通文本中的域名。
//   例如：<p>Visit bp.blogspot.com</p> 中的域名不应该被重写。
//   HTML tokenizer 能精确识别标签和属性，只重写真正需要缓存的地方。
//
// 处理流程：
//   1. 逐个读取 HTML token（开始标签、结束标签、文本等）
//   2. 遇到 <style> 标签时，跟踪嵌套深度，对其内容做 CSS url() 重写
//   3. 遇到其他标签时，查找是否有需要重写的 URL 属性
//   4. 非标签内容（普通文本、注释等）原样输出
//
// 参数：
//   body — 原始 HTML 字节
//
// 返回值：
//   []byte — 重写后的 HTML 字节
func (h *ProxyHandler) rewriteHTML(body []byte, sameSiteHosts []string) []byte {
	var buf bytes.Buffer
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	// 使用嵌套深度计数器（而非布尔值），防止嵌套 <style> 标签导致的异常
	styleDepth := 0

	for {
		tokenType := tokenizer.Next()
		// ErrorToken 通常表示 EOF（正常结束），直接退出循环
		if tokenType == html.ErrorToken {
			break
		}

		switch tokenType {
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			// <style> 开始标签——进入 CSS 上下文
			// 注意：SelfClosingTagToken（如 <style/>）不会触发深度变化
			if token.Data == "style" && tokenType == html.StartTagToken {
				styleDepth++
			}
			// 重写标签中的 URL 属性
			token = h.rewriteTag(token, sameSiteHosts)
			buf.WriteString(token.String())

		case html.EndTagToken:
			token := tokenizer.Token()
			// </style> 结束标签——退出 CSS 上下文
			if token.Data == "style" && styleDepth > 0 {
				styleDepth--
			}
			buf.WriteString(token.String())

		case html.TextToken:
			// 文本内容——如果在 <style> 标签内，需要重写 CSS url()
			// 注意：不能在此分支调用 tokenizer.Token()，因为 Token() 内部
			// 对 TextToken 会调用 Text() → unescape()，后者会原地修改
			// tokenizer 的缓冲区。如果之后调用 tokenizer.Raw()，会返回
			// 原始长度的 span，其中包含 unescape 缩短结果后的残留字节，
			// 导致中文文本中出现字符重复+乱码。
			if styleDepth > 0 {
				buf.WriteString(rewriteCSSUrls(string(tokenizer.Raw()), h.CacheDomains, sameSiteHosts))
			} else {
				buf.Write(tokenizer.Raw())
			}

		default:
			// 注释、DOCTYPE 等——原样输出
			// 同样不能先调用 tokenizer.Token()，原因同上
			buf.Write(tokenizer.Raw())
		}
	}

	return buf.Bytes()
}

// rewriteTag 重写单个 HTML 标签中的 URL 属性。
//
// 处理逻辑：
//   1. 首先处理 style 属性（任何元素都可能有），重写其中的 url()
//   2. 查找 urlAttrs 映射表，获取该标签需要处理的属性列表
//   3. 遍历属性，对 srcset 类属性做特殊处理（多 URL），其余直接重写
//
// 参数：
//   token — HTML tokenizer 解析出的标签 token
//
// 返回值：
//   html.Token — 重写后的标签 token
func (h *ProxyHandler) rewriteTag(token html.Token, sameSiteHosts []string) html.Token {
	tagName := token.Data

	// ---- 处理 style 属性（存在于任何元素上） ----
	for i, attr := range token.Attr {
		if attr.Key == "style" {
			token.Attr[i].Val = rewriteCSSUrls(attr.Val, h.CacheDomains, sameSiteHosts)
		}
	}

	// ---- 处理 <a href> 同站导航链接：绝对链接转为相对路径 ----
	// 这样可避免用户在 http/https 之间跳转，并去掉不必要的域名。
	if tagName == "a" {
		for i, attr := range token.Attr {
			if attr.Key == "href" {
				token.Attr[i].Val = makeSameSiteRelative(attr.Val, sameSiteHosts)
			}
		}
		return token
	}

	// ---- 检查该标签是否有需要处理的资源 URL 属性 ----
	attrs, ok := urlAttrs[tagName]
	if !ok {
		return token // 不在白名单中，无需处理
	}

	// ---- 遍历需要处理的属性 ----
	for _, attrName := range attrs {
		for i, attr := range token.Attr {
			if attr.Key != attrName {
				continue
			}

			switch attrName {
			case "srcset", "data-srcset":
				// srcset 包含多个 URL（用逗号分隔），需要特殊处理
				token.Attr[i].Val = h.rewriteSrcset(attr.Val, sameSiteHosts)
			default:
				// 先尝试同站相对化；如果是同站链接则改为相对路径
				relative := makeSameSiteRelative(attr.Val, sameSiteHosts)
				if relative != attr.Val {
					token.Attr[i].Val = relative
					continue
				}
				// 不是同站链接时，按原来的缓存域名逻辑处理
				parsed, err := url.Parse(attr.Val)
				if err != nil {
					continue // 无效 URL 跳过
				}
				if shouldCacheHost(parsed.Host, h.CacheDomains) {
					token.Attr[i].Val = rewriteURL(attr.Val)
				}
			}
		}
	}

	return token
}

// ServeHTTP 实现 http.Handler 接口，处理所有 HTTP 请求。
//
// 路由逻辑：
//   1. /static-cache/?url=... → 缓存资源请求，交给 HandleCacheRequest 处理
//   2. 其他路径 → 检查域名白名单 → 通过反向代理转发到上游
//
// 参数：
//   w — HTTP 响应写入器
//   r — HTTP 请求
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ---- 路由 1：缓存资源请求 ----
	// 路径以 /static-cache/ 开头，说明是之前重写过的资源链接
	if strings.HasPrefix(r.URL.Path, "/static-cache/") {
		HandleCacheRequest(w, r, h.Config.Server.CacheDir, h.CacheDomains)
		return
	}

	// ---- 路由 2：域名白名单检查 ----
	// 只有配置了 proxy_domains 的域名才允许代理
	domainAllowed := false
	hostName := r.Host
	// 去掉端口号（Host 头可能包含端口，如 "example.com:8080"）
	if strings.Contains(hostName, ":") {
		hostName = strings.Split(hostName, ":")[0]
	}

	for _, d := range h.Config.Server.ProxyDomains {
		if d == hostName {
			domainAllowed = true
			break
		}
	}

	// 白名单非空但当前域名不在其中：拒绝请求
	if !domainAllowed && len(h.Config.Server.ProxyDomains) > 0 {
		http.Error(w, "Domain not allowed by proxy", http.StatusForbidden)
		return
	}

	// ---- 通过反向代理转发到上游 ----
	h.ReverseProxy.ServeHTTP(w, r)
}