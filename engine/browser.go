package engine

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// GlobalUserAgent is the fingerprint target: Microsoft Edge 151 on macOS
// (Intel, OS X 10_15_7), using Chromium's reduced UA version format
// (Edg/151.0.0.0).
const GlobalUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0"

// Low-entropy client hints and headers matching the same Edge-on-macOS
// fingerprint as GlobalUserAgent.
const (
	browserAcceptEncoding = "gzip, deflate, br, zstd"
	browserAcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8"
	browserSecChUa        = `"Not=A?Brand";v="99", "Microsoft Edge";v="151", "Chromium";v="151"`
	browserSecChUaMobile  = "?0"
	browserSecChPlatform  = `"macOS"`
)

func setBrowserHeaders(h http.Header) {
	h.Set("User-Agent", GlobalUserAgent)
	h.Set("Accept-Language", browserAcceptLanguage)
	h.Set("Accept-Encoding", browserAcceptEncoding)
	h.Set("Sec-Ch-Ua", browserSecChUa)
	h.Set("Sec-Ch-Ua-Mobile", browserSecChUaMobile)
	h.Set("Sec-Ch-Ua-Platform", browserSecChPlatform)
}

// setNavigationHeaders mimics a top-level navigation started from the
// address bar (no Referer, Sec-Fetch-Site: none).
func setNavigationHeaders(h http.Header) {
	setBrowserHeaders(h)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
}

// setFetchHeaders mimics a script-initiated request without document context,
// used for HEAD probes.
func setFetchHeaders(h http.Header) {
	setBrowserHeaders(h)
	h.Set("Accept", "*/*")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "none")
}

// browserTransport advertises Edge's Accept-Encoding and transparently
// decodes the response, because net/http only handles gzip itself and would
// otherwise write brotli/zstd bytes verbatim into downloaded files.
type browserTransport struct {
	base http.RoundTripper
}

func newBrowserClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &browserTransport{base: http.DefaultTransport.(*http.Transport).Clone()},
		Timeout:   timeout,
	}
}

func (t *browserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Accept-Encoding", browserAcceptEncoding)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if req.Method == http.MethodHead || resp.Body == nil || resp.Body == http.NoBody {
		return resp, nil
	}

	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	decoded, ok := decodeResponseBody(encoding, resp.Body)
	if !ok {
		return resp, nil
	}

	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	resp.Body = decoded
	return resp, nil
}

func decodeResponseBody(encoding string, body io.ReadCloser) (io.ReadCloser, bool) {
	switch encoding {
	case "gzip":
		r, err := gzip.NewReader(body)
		if err != nil {
			return nil, false
		}
		return newBodyReader(r, r, body), true
	case "br":
		return newBodyReader(brotli.NewReader(body), body), true
	case "zstd":
		r, err := zstd.NewReader(body)
		if err != nil {
			return nil, false
		}
		return newBodyReader(r, body, voidCloser{r.Close}), true
	case "deflate":
		// Some servers send raw deflate despite the zlib framing the
		// "deflate" coding implies; probe the zlib header through a
		// buffered reader so no bytes are lost on fallback.
		br := bufio.NewReader(body)
		if zr, err := zlib.NewReader(br); err == nil {
			return newBodyReader(zr, body), true
		}
		return newBodyReader(flate.NewReader(br), body), true
	}
	return nil, false
}

func newBodyReader(r io.Reader, toClose ...io.Closer) io.ReadCloser {
	return &bodyReader{Reader: r, toClose: toClose}
}

type bodyReader struct {
	io.Reader
	toClose []io.Closer
}

func (b *bodyReader) Close() error {
	var firstErr error
	for _, c := range b.toClose {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// voidCloser adapts closers like zstd.Decoder whose Close has no return
// value to io.Closer.
type voidCloser struct{ close func() }

func (v voidCloser) Close() error {
	v.close()
	return nil
}
