// =============================================================================
// 代理与 HTML 重写测试
// =============================================================================
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestHandler 创建一个用于测试的 ProxyHandler 实例。
// 使用典型的 Blogger 缓存域名列表。
func newTestHandler() *ProxyHandler {
	return &ProxyHandler{
		CacheDomains: []string{
			"bp.blogspot.com",
			"resources.blogblog.com",
			"fonts.googleapis.com",
			"fonts.gstatic.com",
			"lh3.googleusercontent.com",
		},
	}
}

// TestShouldCacheHost 验证域名匹配逻辑：
//   - 精确匹配
//   - 子域名匹配
//   - 不匹配的域名（无关域名、部分匹配）
func TestShouldCacheHost(t *testing.T) {
	h := newTestHandler()

	tests := []struct {
		host     string // 输入主机名
		expected bool   // 是否应该缓存
	}{
		{"bp.blogspot.com", true},                       // 精确匹配
		{"fonts.googleapis.com", true},                  // 精确匹配
		{"lh3.googleusercontent.com", true},             // 精确匹配
		{"sub.lh3.googleusercontent.com", true},         // 子域名匹配
		{"example.com", false},                          // 不在列表中
		{"google.com", false},                           // 不在列表中
		{"blogspot.com", false},                         // 部分匹配不算（不是子域名）
	}

	for _, tt := range tests {
		got := shouldCacheHost(tt.host, h.CacheDomains)
		if got != tt.expected {
			t.Errorf("shouldCacheHost(%q) = %v, want %v", tt.host, got, tt.expected)
		}
	}
}

// TestRewriteURL 验证 URL 重写功能：
//   - 绝对 URL → /static-cache/?url=base64...
//   - 协议相对 URL → 先补全 https: 再编码
//   - 产生的 URL 是合法的
func TestRewriteURL(t *testing.T) {
	tests := []struct {
		input    string // 原始 URL
		contains string // 重写后应包含的字符串
	}{
		{"https://bp.blogspot.com/image.png", "/static-cache/?url="},
		{"//fonts.googleapis.com/css?family=Roboto", "/static-cache/?url="},
	}

	for _, tt := range tests {
		got := rewriteURL(tt.input)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("rewriteURL(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
		}
		// 验证生成的 URL 是合法的
		parsed, err := url.Parse(got)
		if err != nil {
			t.Errorf("rewriteURL(%q) produced invalid URL %q: %v", tt.input, got, err)
		}
		if parsed.Path != "/static-cache/" {
			t.Errorf("rewriteURL(%q) path = %q, want /static-cache/", tt.input, parsed.Path)
		}
	}
}

// TestMakeSameSiteRelative 验证同站链接转相对路径的逻辑。
func TestMakeSameSiteRelative(t *testing.T) {
	tests := []struct {
		input    string   // 原始 href
		hosts    []string // 视为同一站点的主机名
		expected string   // 期望输出
	}{
		{"https://www.my-blog.com/2024/01/post.html", []string{"www.my-blog.com"}, "/2024/01/post.html"},
		{"http://www.my-blog.com/post", []string{"www.my-blog.com"}, "/post"},
		{"//www.my-blog.com/post", []string{"www.my-blog.com"}, "/post"},
		{"https://www.my-blog.com/post?x=1#top", []string{"www.my-blog.com"}, "/post?x=1#top"},
		{"https://www.my-blog.com", []string{"www.my-blog.com"}, "/"},
		{"https://www.my-blog.com?foo=bar", []string{"www.my-blog.com"}, "/?foo=bar"},
		{"https://www.my-blog.com:8080/post", []string{"www.my-blog.com"}, "/post"},
		{"https://WWW.My-Blog.COM/post", []string{"www.my-blog.com"}, "/post"},
		{"https://other-blog.com/post", []string{"www.my-blog.com"}, "https://other-blog.com/post"},
		{"https://sub.my-blog.com/post", []string{"www.my-blog.com"}, "https://sub.my-blog.com/post"},
		{"/2024/01/post.html", []string{"www.my-blog.com"}, "/2024/01/post.html"},
		{"./post.html", []string{"www.my-blog.com"}, "./post.html"},
		{"../post.html", []string{"www.my-blog.com"}, "../post.html"},
		{"#section", []string{"www.my-blog.com"}, "#section"},
		{"mailto:test@example.com", []string{"www.my-blog.com"}, "mailto:test@example.com"},
		{"javascript:void(0)", []string{"www.my-blog.com"}, "javascript:void(0)"},
		{"tel:+1234567890", []string{"www.my-blog.com"}, "tel:+1234567890"},
		{"data:text/plain,hello", []string{"www.my-blog.com"}, "data:text/plain,hello"},
		{"https://m.my-blog.com/post", []string{"www.my-blog.com", "m.my-blog.com"}, "/post"},
		{"", []string{"www.my-blog.com"}, ""},
	}

	for _, tt := range tests {
		got := makeSameSiteRelative(tt.input, tt.hosts)
		if got != tt.expected {
			t.Errorf("makeSameSiteRelative(%q, %v) = %q, want %q", tt.input, tt.hosts, got, tt.expected)
		}
	}
}

// TestRewriteSrcset 验证 srcset 属性重写：
//   - 所有匹配缓存域名的 URL 被重写
//   - 不匹配的 URL 保持原样
//   - 混合场景正确处理
func TestRewriteSrcset(t *testing.T) {
	h := newTestHandler()

	tests := []struct {
		input    string // 原始 srcset
		contains string // 应包含的字符串
		notCont  string // 不应包含的字符串
	}{
		{
			// 全部缓存域名 → 全部重写
			"https://bp.blogspot.com/img1.jpg 1x, https://bp.blogspot.com/img2.jpg 2x",
			"/static-cache/?url=",
			"bp.blogspot.com", // 原始域名不应出现
		},
		{
			// 非缓存域名 → 保持原样
			"https://example.com/not-cached.jpg 1x",
			"example.com",
			"/static-cache/",
		},
		{
			// 混合：缓存域名 + 非缓存域名
			"https://bp.blogspot.com/small.jpg 480w, https://example.com/other.jpg 800w",
			"/static-cache/",
			"bp.blogspot.com", // 缓存域名的原始 URL 不应出现
		},
	}

	for _, tt := range tests {
		got := h.rewriteSrcset(tt.input, []string{"www.my-blog.com"})
		if !strings.Contains(got, tt.contains) {
			t.Errorf("rewriteSrcset(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
		}
		if tt.notCont != "" && strings.Contains(got, tt.notCont) {
			t.Errorf("rewriteSrcset(%q) = %q, should NOT contain %q", tt.input, got, tt.notCont)
		}
	}
}

// TestRewriteCSSUrls 验证 CSS url() 引用重写：
//   - url(https://...) 无引号格式
//   - url("https://...") 双引号格式
//   - url('https://...') 单引号格式
//   - 非缓存域名不重写
//   - data: URL 不重写
func TestRewriteCSSUrls(t *testing.T) {
	h := newTestHandler()

	tests := []struct {
		input    string // CSS 片段
		contains string // 应包含的字符串
		notCont  string // 不应包含的字符串
	}{
		{
			`url(https://fonts.googleapis.com/css?family=Roboto)`,
			"/static-cache/?url=",
			"fonts.googleapis.com",
		},
		{
			`url("https://fonts.gstatic.com/font.woff2")`,
			"/static-cache/?url=",
			"fonts.gstatic.com",
		},
		{
			`url('https://lh3.googleusercontent.com/image.png')`,
			"/static-cache/?url=",
			"lh3.googleusercontent.com",
		},
		{
			// 非缓存域名：保持不变
			`url(https://example.com/bg.jpg)`,
			"example.com",
			"/static-cache/",
		},
		{
			// data: URL 不应被重写（解析后 host 为空）
			`url(data:image/png;base64,abc)`,
			"data:image/png",
			"/static-cache/",
		},
		{
			// 真实 Google Fonts CSS：@font-face 中的 src url()
			`@font-face { font-family: 'Roboto'; src: url(https://fonts.gstatic.com/s/roboto/v30/abc.woff2) format('woff2'); }`,
			"/static-cache/?url=",
			"fonts.gstatic.com",
		},
		{
			// 混合场景：缓存域名 + 非缓存域名共存
			`body { background: url(https://lh3.googleusercontent.com/bg.jpg); } .other { bg: url(https://example.com/other.jpg); }`,
			"/static-cache/?url=",
			"lh3.googleusercontent.com",
		},
		{
			// 协议相对 URL 在 CSS 中
			`url(//fonts.gstatic.com/s/roboto/v30/abc.woff2)`,
			"/static-cache/?url=",
			"fonts.gstatic.com",
		},
		{
			// 本站 URL：转为相对路径
			`url(https://www.my-blog.com/bg.jpg)`,
			"url(/bg.jpg)",
			"www.my-blog.com",
		},
		{
			// 协议相对本站 URL：转为相对路径
			`url(//www.my-blog.com/bg.jpg)`,
			"url(/bg.jpg)",
			"//www.my-blog.com",
		},
	}

	for _, tt := range tests {
		got := rewriteCSSUrls(tt.input, h.CacheDomains, []string{"www.my-blog.com"})
		if !strings.Contains(got, tt.contains) {
			t.Errorf("rewriteCSSUrls(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
		}
		if tt.notCont != "" && strings.Contains(got, tt.notCont) {
			t.Errorf("rewriteCSSUrls(%q) = %q, should NOT contain %q", tt.input, got, tt.notCont)
		}
	}
}

// TestRewriteHTML_Basic 验证 HTML 重写的各种场景。
// 使用表驱动测试，每个用例覆盖一种 HTML 元素或边界情况。
func TestRewriteHTML_Basic(t *testing.T) {
	h := newTestHandler()

	tests := []struct {
		name     string // 测试名称
		input    string // 输入 HTML 片段
		contains string // 重写后应包含的内容
		notCont  string // 重写后不应包含的内容
	}{
		{
			name:     "link href",                    // 样式表链接
			input:    `<link rel="stylesheet" href="https://fonts.googleapis.com/css?family=Roboto">`,
			contains: `/static-cache/?url=`,
			notCont:  `fonts.googleapis.com`,
		},
		{
			name:     "script src",                   // 脚本引用
			input:    `<script src="https://resources.blogblog.com/blogblog.js"></script>`,
			contains: `/static-cache/?url=`,
			notCont:  `resources.blogblog.com`,
		},
		{
			name:     "img src",                      // 图片引用
			input:    `<img src="https://lh3.googleusercontent.com/photo.jpg">`,
			contains: `/static-cache/?url=`,
			notCont:  `lh3.googleusercontent.com`,
		},
		{
			name:     "img srcset",                   // 响应式图片
			input:    `<img srcset="https://bp.blogspot.com/small.jpg 480w, https://bp.blogspot.com/large.jpg 1024w">`,
			contains: `/static-cache/?url=`,
			notCont:  `bp.blogspot.com`,
		},
		{
			name:     "non-cached domain untouched",  // 非缓存域名不重写
			input:    `<img src="https://example.com/photo.jpg">`,
			contains: `example.com`,
			notCont:  `/static-cache/`,
		},
		{
			name:     "inline style with url",        // 内联样式中的 url()
			input:    `<div style="background: url(https://lh3.googleusercontent.com/bg.jpg)"></div>`,
			contains: `/static-cache/?url=`,
			notCont:  `lh3.googleusercontent.com`,
		},
		{
			name:     "style tag content",            // <style> 标签中的 url()
			input:    `<style>body { background: url(https://bp.blogspot.com/bg.png); }</style>`,
			contains: `/static-cache/?url=`,
			notCont:  `bp.blogspot.com`,
		},
		{
			name:     "protocol-relative URL",        // 协议相对 URL (//)
			input:    `<script src="//resources.blogblog.com/blogblog.js"></script>`,
			contains: `/static-cache/?url=`,
			notCont:  `resources.blogblog.com`,
		},
		{
			name:     "non-URL text preserved",       // 普通文本中的域名不重写
			input:    `<p>Visit fonts.googleapis.com for fonts</p>`,
			contains: `fonts.googleapis.com`,         // 文本中的域名应保持原样
			notCont:  `/static-cache/`,
		},
		{
			name:     "a href not rewritten to static cache", // <a> 同站链接会转相对路径，但不会被当作缓存资源
			input:    `<a href="https://bp.blogspot.com/post">Read more</a>`,
			contains: `bp.blogspot.com`,
			notCont:  `/static-cache/`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(h.rewriteHTML([]byte(tt.input), []string{"www.my-blog.com"}))
			if !strings.Contains(got, tt.contains) {
				t.Errorf("rewriteHTML(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
			}
			if tt.notCont != "" && strings.Contains(got, tt.notCont) {
				t.Errorf("rewriteHTML(%q) = %q, should NOT contain %q", tt.input, got, tt.notCont)
			}
		})
	}
}

// TestRewriteHTML_FullDocument 验证完整 HTML 文档的重写。
// 涵盖了多种标签混合、CSS 样式、普通文本、导航链接等组合场景。
func TestRewriteHTML_FullDocument(t *testing.T) {
	h := newTestHandler()

	// 构造一个完整的 Blogger 博客页面 HTML 片段
	input := `<!DOCTYPE html>
<html>
<head>
	<link rel="stylesheet" href="https://fonts.googleapis.com/css?family=Roboto">
	<script src="//resources.blogblog.com/blogblog.js"></script>
	<style>
		body { background: url(https://bp.blogspot.com/bg.png); }
	</style>
</head>
<body>
	<img src="https://lh3.googleusercontent.com/photo.jpg" srcset="https://lh3.googleusercontent.com/small.jpg 480w">
	<img src="https://example.com/other.jpg">
	<a href="https://www.blogger.com/post">Link</a>
	<p>Plain text mentioning bp.blogspot.com should stay untouched</p>
</body>
</html>`

	got := string(h.rewriteHTML([]byte(input), []string{"www.my-blog.com"}))

	// 应包含的内容
	mustContain := []string{
		"/static-cache/?url=",                                              // 缓存域名被重写了
		"example.com/other.jpg",                                            // 非缓存域名保持原样
		"www.blogger.com/post",                                             // <a> 链接保持原样
		"Plain text mentioning bp.blogspot.com",                            // 文本中的域名保持原样
	}

	// 不应包含的内容（原始缓存域名 URL 不应保留）
	mustNotContain := []string{
		`href="https://fonts.googleapis.com/`,                              // link href 不应保留原始 URL
		`src="https://resources.blogblog.com/`,                             // script src 不应保留原始 URL
		`src="https://lh3.googleusercontent.com/`,                          // img src 不应保留原始 URL
		`srcset="https://lh3.googleusercontent.com/`,                       // img srcset 不应保留原始 URL
		`url(https://bp.blogspot.com/`,                                     // CSS url() 不应保留原始 URL
	}

	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("rewritten HTML missing expected content: %q", s)
		}
	}
	for _, s := range mustNotContain {
		if strings.Contains(got, s) {
			t.Errorf("rewritten HTML contains unexpected raw URL: %q", s)
		}
	}
}

// TestRewriteHTML_SameSiteLinks 验证同站 <a href> 导航链接被重写为相对路径，
// 而外部链接、锚点、特殊协议和已相对路径保持原样。
func TestRewriteHTML_SameSiteLinks(t *testing.T) {
	h := newTestHandler()

	tests := []struct {
		name     string // 测试名称
		input    string // 输入 HTML 片段
		contains string // 重写后应包含的内容
		notCont  string // 重写后不应包含的内容
	}{
		{
			name:     "https same-site link",
			input:    `<a href="https://www.my-blog.com/2024/01/post.html">Read more</a>`,
			contains: `href="/2024/01/post.html"`,
			notCont:  `https://www.my-blog.com`,
		},
		{
			name:     "http same-site link",
			input:    `<a href="http://www.my-blog.com/post">Read more</a>`,
			contains: `href="/post"`,
			notCont:  `http://www.my-blog.com`,
		},
		{
			name:     "protocol-relative same-site link",
			input:    `<a href="//www.my-blog.com/post">Read more</a>`,
			contains: `href="/post"`,
			notCont:  `//www.my-blog.com`,
		},
		{
			name:     "same-site link with query and fragment",
			input:    `<a href="https://www.my-blog.com/post?x=1#top">Read more</a>`,
			contains: `href="/post?x=1#top"`,
		},
		{
			name:     "same-site link with other attributes preserved",
			input:    `<a class="btn" href="https://www.my-blog.com/post">Read more</a>`,
			contains: `class="btn"`,
			notCont:  `https://www.my-blog.com`,
		},
		{
			name:     "external link unchanged",
			input:    `<a href="https://other.com/post">External</a>`,
			contains: `href="https://other.com/post"`,
		},
		{
			name:     "relative link unchanged",
			input:    `<a href="/local/post">Local</a>`,
			contains: `href="/local/post"`,
		},
		{
			name:     "anchor unchanged",
			input:    `<a href="#section">Section</a>`,
			contains: `href="#section"`,
		},
		{
			name:     "mailto unchanged",
			input:    `<a href="mailto:a@b.com">Email</a>`,
			contains: `href="mailto:a@b.com"`,
		},
		{
			name:     "javascript unchanged",
			input:    `<a href="javascript:void(0)">Action</a>`,
			contains: `href="javascript:void(0)"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(h.rewriteHTML([]byte(tt.input), []string{"www.my-blog.com"}))
			if !strings.Contains(got, tt.contains) {
				t.Errorf("rewriteHTML(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
			}
			if tt.notCont != "" && strings.Contains(got, tt.notCont) {
				t.Errorf("rewriteHTML(%q) = %q, should NOT contain %q", tt.input, got, tt.notCont)
			}
		})
	}
}

// TestRewriteHTML_SameSiteResources 验证 CSS/JS 相关的同站资源链接被重写为相对路径，
// 而外部资源仍按原来的缓存逻辑处理。
func TestRewriteHTML_SameSiteResources(t *testing.T) {
	h := newTestHandler()

	tests := []struct {
		name     string // 测试名称
		input    string // 输入 HTML 片段
		contains string // 重写后应包含的内容
		notCont  string // 重写后不应包含的内容
	}{
		{
			name:     "same-site stylesheet link",
			input:    `<link rel="stylesheet" href="https://www.my-blog.com/css/style.css">`,
			contains: `href="/css/style.css"`,
			notCont:  `https://www.my-blog.com`,
		},
		{
			name:     "same-site script src",
			input:    `<script src="https://www.my-blog.com/js/app.js"></script>`,
			contains: `src="/js/app.js"`,
			notCont:  `https://www.my-blog.com`,
		},
		{
			name:     "same-site protocol-relative stylesheet",
			input:    `<link rel="stylesheet" href="//www.my-blog.com/css/style.css">`,
			contains: `href="/css/style.css"`,
			notCont:  `//www.my-blog.com`,
		},
		{
			name:     "external stylesheet still cached",
			input:    `<link rel="stylesheet" href="https://fonts.googleapis.com/css?family=Roboto">`,
			contains: `/static-cache/?url=`,
			notCont:  `fonts.googleapis.com`,
		},
		{
			name:     "external script still cached",
			input:    `<script src="https://resources.blogblog.com/blogblog.js"></script>`,
			contains: `/static-cache/?url=`,
			notCont:  `resources.blogblog.com`,
		},
		{
			name:     "inline style same-site url",
			input:    `<div style="background: url(https://www.my-blog.com/bg.jpg)"></div>`,
			contains: `url(/bg.jpg)`,
			notCont:  `https://www.my-blog.com`,
		},
		{
			name:     "style tag same-site url",
			input:    `<style>body { background: url(https://www.my-blog.com/bg.jpg); }</style>`,
			contains: `url(/bg.jpg)`,
			notCont:  `https://www.my-blog.com`,
		},
		{
			name:     "same-site img src",
			input:    `<img src="https://www.my-blog.com/photo.jpg">`,
			contains: `src="/photo.jpg"`,
			notCont:  `https://www.my-blog.com`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(h.rewriteHTML([]byte(tt.input), []string{"www.my-blog.com"}))
			if !strings.Contains(got, tt.contains) {
				t.Errorf("rewriteHTML(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
			}
			if tt.notCont != "" && strings.Contains(got, tt.notCont) {
				t.Errorf("rewriteHTML(%q) = %q, should NOT contain %q", tt.input, got, tt.notCont)
			}
		})
	}
}

// TestRewriteHTML_EmptyBody 验证：空输入不会崩溃。
func TestRewriteHTML_EmptyBody(t *testing.T) {
	h := newTestHandler()
	got := string(h.rewriteHTML([]byte(""), []string{"www.my-blog.com"}))
	if got != "" {
		t.Errorf("expected empty output, got %q", got)
	}
}

// TestRewriteHTML_EmojiPreservation 验证：HTML 中的 emoji 表情符号在重写后保持不变。
// 这验证了 utf8.Valid 前置检查的有效性——合法 UTF-8 内容（包括 emoji）应原样保留。
func TestRewriteHTML_EmojiPreservation(t *testing.T) {
	h := newTestHandler()

	// 测试多种 emoji 类型：
	//   - 基本 emoji（😀 U+1F600, 4 字节 UTF-8）
	//   - 中文 + emoji 混合
	//   - 国旗 emoji（🇨🇳 U+1F1E8 U+1F1F3, 8 字节序列）
	//   - 肤色修饰 emoji（👍🏻 U+1F44D U+1F3FB, ZWJ 序列）
	input := `<html><body>
	<p>Hello World 😀🎉</p>
	<p>这是一段中文文本，带有 emoji 表情 😊❤️🔥</p>
	<p>Flag: 🇨🇳</p>
	<p>Thumbs up: 👍🏻</p>
	<p>Family: 👨‍👩‍👧‍👦</p>
	<img src="https://lh3.googleusercontent.com/photo.jpg">
</body></html>`

	got := string(h.rewriteHTML([]byte(input), []string{"www.my-blog.com"}))

	// 验证所有 emoji 都保留
	emojis := []string{
		"😀", "🎉",
		"😊", "❤️", "🔥",
		"🇨🇳",
		"👍🏻",
		"👨‍👩‍👧‍👦",
		"这是一段中文文本，带有 emoji 表情",
	}

	for _, emoji := range emojis {
		if !strings.Contains(got, emoji) {
			t.Errorf("emoji %q was lost or corrupted during HTML rewriting", emoji)
		}
	}

	// 验证资源 URL 被正确重写（emoji 不应影响重写逻辑）
	if !strings.Contains(got, "/static-cache/?url=") {
		t.Error("resource URL was not rewritten")
	}
	if strings.Contains(got, "lh3.googleusercontent.com") {
		t.Error("original resource URL should have been rewritten")
	}
}

// TestRewriteHTML_EntityInChineseText 验证：HTML 实体（如 &nbsp;）在中文文本中不会导致乱码或字符重复。
// 这是针对 tokenizer.Raw() 在 Token() 之后被调用导致缓冲区污染的回归测试。
func TestRewriteHTML_EntityInChineseText(t *testing.T) {
	h := newTestHandler()

	// 使用 &nbsp;（6字节）→ NBSP（2字节），会产生缓冲区残留数据
	input := `<p>你好&nbsp;世界，欢迎来到我的博客。</p>`
	got := string(h.rewriteHTML([]byte(input), []string{"www.my-blog.com"}))
	if got != input {
		t.Errorf("expected unchanged HTML, got %q", got)
	}
}

// TestRewriteHTML_EntityAtBoundary 验证：实体出现在中文字符边界处不会导致字符重复。
// 例如：中&nbsp;文 → 不会被错误地重写成 "中 文文"。
func TestRewriteHTML_EntityAtBoundary(t *testing.T) {
	h := newTestHandler()

	input := `<p>中&nbsp;文</p>`
	got := string(h.rewriteHTML([]byte(input), []string{"www.my-blog.com"}))
	if got != input {
		t.Errorf("expected unchanged HTML, got %q", got)
	}
}

// TestRewriteHTML_MultipleEntitiesInChinese 验证：多个 HTML 实体连续出现在中文文本中都能正确处理。
func TestRewriteHTML_MultipleEntitiesInChinese(t *testing.T) {
	h := newTestHandler()

	input := `<p>A&amp;B&nbsp;测试&lt;HTML&gt;&quot;引用&quot;结束。</p>`
	got := string(h.rewriteHTML([]byte(input), []string{"www.my-blog.com"}))
	if got != input {
		t.Errorf("expected unchanged HTML, got %q", got)
	}
}

// TestRewriteHTML_CRInChineseText 验证：回车符在中文文本中不会导致字节残留。
func TestRewriteHTML_CRInChineseText(t *testing.T) {
	h := newTestHandler()

	input := "<p>中文\r文本\r\n换行</p>"
	got := string(h.rewriteHTML([]byte(input), []string{"www.my-blog.com"}))
	// Tokenizer.Raw() 返回原始字节，所以回车应保留
	if got != input {
		t.Errorf("expected unchanged HTML, got %q", got)
	}
}

// TestModifyResponse_PassesRequestHost 验证 modifyResponse 能将当前请求域名
// 传递到 HTML 重写流程，使同站 <a href> 链接被正确转为相对路径。
func TestModifyResponse_PassesRequestHost(t *testing.T) {
	h := newTestHandler()

	req := httptest.NewRequest("GET", "http://www.my-blog.com/", nil)
	req.Host = "www.my-blog.com"

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(`<a href="https://www.my-blog.com/post">Link</a>`)),
		Request:    req,
	}

	if err := h.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse failed: %v", err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	got := string(body)
	if !strings.Contains(got, `href="/post"`) {
		t.Errorf("expected same-site link to become relative, got %q", got)
	}
	if strings.Contains(got, "https://www.my-blog.com") {
		t.Errorf("original absolute URL should have been removed, got %q", got)
	}
}