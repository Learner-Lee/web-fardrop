package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/style.css
var styleCSS []byte

//go:embed web/app.js
var appJS []byte

//go:embed web/share.html
var shareHTML string

//go:embed web/cover.png
var coverPNG []byte

var sharePage = template.Must(template.New("share").Parse(shareHTML))

type edge struct {
	upstream  *url.URL
	secret    string
	client    *http.Client
	api       *httputil.ReverseProxy
	cache     *blockCache
	downloads chan struct{}
	uploads   chan struct{}
	tunnel    *originTunnel
}

func newEdge(upstream *url.URL, secret string, budget int) *edge {
	t := &http.Transport{
		DialContext:       (&net.Dialer{Timeout: 6 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 24,
		MaxConnsPerHost: 48, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 6 * time.Second, DisableCompression: true,
	}
	e := &edge{
		upstream: upstream, secret: secret, client: &http.Client{Transport: t,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		downloads: make(chan struct{}, 16), uploads: make(chan struct{}, 4),
	}
	if upstream.Host == tunnelHost {
		e.tunnel = &originTunnel{secret: secret}
		t.DialContext = e.tunnel.dial
	}
	e.api = &httputil.ReverseProxy{
		Transport: t,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, strings.TrimSuffix(base, "/"))
			p.Out.URL.RawPath = ""
			p.Out.Header.Set(edgeHeader, secret)
			p.Out.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			problem(w, http.StatusBadGateway, "存储服务器暂时无法连接。")
		},
	}
	e.cache = &blockCache{blocks: make(map[blockKey]*cachedBlock), space: make(chan struct{}),
		residentLimit: budget, inflightLimit: 128 << 20, fetch: e.fetch}
	return e
}

func (e *edge) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"{$}", static(indexHTML, "text/html; charset=utf-8"))
	mux.HandleFunc("GET "+base+"style.css", static(styleCSS, "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+base+"app.js", static(appJS, "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET "+base+"cover.png", static(coverPNG, "image/png"))
	mux.HandleFunc("GET "+base+"s/{token}", e.share)
	mux.HandleFunc("GET "+base+"d/{token}", e.download)
	mux.HandleFunc(base+"api/", e.admin)
	mux.HandleFunc("GET /healthz", e.health)
	if e.tunnel != nil {
		mux.HandleFunc("GET "+tunnelPath, e.tunnel.accept)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "private, no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'; object-src 'none'")
		mux.ServeHTTP(w, r)
	})
}

func static(content []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}
}

func (e *edge) request(ctx context.Context, method, path string, payload io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.upstream.String()+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set(edgeHeader, e.secret)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (e *edge) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	req, err := e.request(ctx, http.MethodGet, "/internal/health", nil)
	if err == nil {
		var resp *http.Response
		resp, err = e.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	problem(w, http.StatusBadGateway, "Origin unavailable")
}

func (e *edge) admin(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !validID(token, 32) {
		problem(w, http.StatusUnauthorized, "请输入有效的管理令牌。")
		return
	}
	if r.Method == http.MethodPut {
		select {
		case e.uploads <- struct{}{}:
			defer func() { <-e.uploads }()
		default:
			problem(w, http.StatusTooManyRequests, "上传繁忙，请稍后重试。")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, legacyUploadChunkSize)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	e.api.ServeHTTP(w, r.WithContext(ctx))
}

func (e *edge) grant(ctx context.Context, token string) (Grant, int) {
	var grant Grant
	if !validID(token, 32) {
		return grant, http.StatusNotFound
	}
	data, _ := json.Marshal(map[string]string{"token": token})
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := e.request(ctx, http.MethodPost, "/internal/authorize", bytes.NewReader(data))
	if err != nil {
		return grant, http.StatusBadGateway
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return grant, http.StatusBadGateway
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return grant, resp.StatusCode
	case http.StatusOK:
		if json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&grant) == nil &&
			validID(grant.File.ID, 16) && validID(grant.File.SHA256, 32) && grant.File.Size > 0 && grant.File.Size <= maxSize {
			return grant, http.StatusOK
		}
	}
	return grant, http.StatusBadGateway
}

func grantMessage(status int) string {
	switch status {
	case http.StatusGone:
		return "此分享链接已过期或已撤销。"
	case http.StatusNotFound:
		return "找不到此分享链接。"
	default:
		return "存储服务器暂时无法连接，请稍后重试。"
	}
}

func sizeLabel(n int64) string {
	for _, unit := range []struct {
		size int64
		name string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if n >= unit.size {
			return fmt.Sprintf("%.2f %s", float64(n)/float64(unit.size), unit.name)
		}
	}
	return strconv.FormatInt(n, 10) + " B"
}

func (e *edge) share(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	g, status := e.grant(r.Context(), token)
	page := struct {
		Name, Size, Expires, Hash, Link, Error string
	}{}
	if status == http.StatusOK {
		page.Name = g.File.Name
		page.Size = sizeLabel(g.File.Size)
		page.Expires = time.Unix(g.ExpiresAt, 0).In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02 15:04 UTC+8")
		page.Hash = g.File.SHA256
		page.Link = base + "d/" + token
	} else {
		page.Error = grantMessage(status)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = sharePage.Execute(w, page)
}

func (e *edge) download(w http.ResponseWriter, r *http.Request) {
	select {
	case e.downloads <- struct{}{}:
		defer func() { <-e.downloads }()
	default:
		problem(w, http.StatusTooManyRequests, "当前下载人数较多，请稍后重试。")
		return
	}
	g, status := e.grant(r.Context(), r.PathValue("token"))
	if status != http.StatusOK {
		problem(w, status, grantMessage(status))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": g.File.Name}))
	w.Header().Set("ETag", `"`+g.File.SHA256+`"`)
	w.Header().Set("X-Accel-Buffering", "no")
	d := &download{ctx: r.Context(), file: g.File, cache: e.cache}
	defer d.Close()
	http.ServeContent(w, r, "", time.Unix(g.File.CreatedAt, 0), d)
}

func (e *edge) fetch(ctx context.Context, key blockKey, size int, appendBytes func([]byte)) error {
	req, err := e.request(ctx, http.MethodGet, "/internal/objects/"+key.object, nil)
	if err != nil {
		return err
	}
	start := key.index * cacheChunkSize
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+int64(size)-1))
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	prefix := fmt.Sprintf("bytes %d-%d/", start, start+int64(size)-1)
	if resp.StatusCode != http.StatusPartialContent || resp.ContentLength != int64(size) || !strings.HasPrefix(resp.Header.Get("Content-Range"), prefix) {
		return fmt.Errorf("invalid origin block status %d", resp.StatusCode)
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			appendBytes(buf[:n])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
