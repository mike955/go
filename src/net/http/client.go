// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// HTTP client. See RFC 7230 through 7235.
//
// This is the high-level Client interface.
// The low-level implementation is in transport.go.

package http

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/internal/ascii"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

type Client struct { // http client 结构体，默认使用 DefaultClient 对象
	Transport RoundTripper // 发送 http 请求的实体，处理 http 请求相关细节，默认使用 DefaultTransport

	CheckRedirect func(req *Request, via []*Request) error // 处理重定向，via 表示已经发出去的请求，req 表示即将到来的请求，checkredirect 为 nil，则每次请求默认重试 10 次

	Jar CookieJar // 处理 cookie 相关操作，默认发送数据不携带 cookie

	Timeout time.Duration // http 请求超时时间，该超时时间为单个 http 请求全生命周期所消耗的时间，包括连接创建、发送、读取响应等
}

// 默认 http client，http 包的 Get、Head、Post 方法默认使用该 client，* 该默认 client 未设置超时时间，可能导致请求一直进行，但没有返回 *
var DefaultClient = &Client{}

type RoundTripper interface { // RoundTripper 定义发送和接收 http 请求，RoundTripper 应该是并发安全的
	RoundTrip(*Request) (*Response, error) // RoundTrip 发送 Requet，读取响应，将数据封装进 Response
}

// refererForURL returns a referer without any authentication info or
// an empty string if lastReq scheme is https and newReq scheme is http.
func refererForURL(lastReq, newReq *url.URL) string {
	// https://tools.ietf.org/html/rfc7231#section-5.5.2
	//   "Clients SHOULD NOT include a Referer header field in a
	//    (non-secure) HTTP request if the referring page was
	//    transferred with a secure protocol."
	if lastReq.Scheme == "https" && newReq.Scheme == "http" {
		return ""
	}
	referer := lastReq.String()
	if lastReq.User != nil {
		// This is not very efficient, but is the best we can
		// do without:
		// - introducing a new method on URL
		// - creating a race condition
		// - copying the URL struct manually, which would cause
		//   maintenance problems down the line
		auth := lastReq.User.String() + "@"
		referer = strings.Replace(referer, auth, "", 1)
	}
	return referer
}

// client 的 send 方法，处理 cookie，调用 send 函数发送请求
func (c *Client) send(req *Request, deadline time.Time) (resp *Response, didTimeout func() bool, err error) {
	if c.Jar != nil {
		for _, cookie := range c.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	resp, didTimeout, err = send(req, c.transport(), deadline)
	if err != nil {
		return nil, didTimeout, err
	}
	if c.Jar != nil {
		if rc := resp.Cookies(); len(rc) > 0 {
			c.Jar.SetCookies(req.URL, rc)
		}
	}
	return resp, nil, nil
}

// 返回超时时间点
func (c *Client) deadline() time.Time {
	if c.Timeout > 0 {
		return time.Now().Add(c.Timeout)
	}
	return time.Time{}
}

// 返回 client 的 Transport
func (c *Client) transport() RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	return DefaultTransport
}

// 发送 http 请求，调用方应该主动关闭 resp.body
func send(ireq *Request, rt RoundTripper, deadline time.Time) (resp *Response, didTimeout func() bool, err error) {
	req := ireq // req is either the original request, or a modified fork

	if rt == nil {
		req.closeBody()
		return nil, alwaysFalse, errors.New("http: no Client.Transport or DefaultTransport")
	}

	if req.URL == nil {
		req.closeBody()
		return nil, alwaysFalse, errors.New("http: nil Request.URL")
	}

	if req.RequestURI != "" {
		req.closeBody()
		return nil, alwaysFalse, errors.New("http: Request.RequestURI can't be set in client requests")
	}

	// forkReq forks req into a shallow clone of ireq the first
	// time it's called.
	forkReq := func() { // req 浅拷贝 ireq, 为啥？
		if ireq == req {
			req = new(Request)
			*req = *ireq // shallow clone
		}
	}

	if req.Header == nil { // 确保请求头被初始化
		forkReq()
		req.Header = make(Header)
	}

	if u := req.URL.User; u != nil && req.Header.Get("Authorization") == "" { // 当 header 设置了用户名和密码，且无 Authorization 头时，添加 Authorization 头，不重写 Authorization 头
		username := u.Username()
		password, _ := u.Password()
		forkReq()
		req.Header = cloneOrMakeHeader(ireq.Header)
		req.Header.Set("Authorization", "Basic "+basicAuth(username, password))
	}

	if !deadline.IsZero() {
		forkReq()
	}
	stopTimer, didTimeout := setRequestCancel(req, rt, deadline) // 启动 goroutine 监听请求超时

	resp, err = rt.RoundTrip(req) // 调用 transport 的 RoundTrip 方法发送请求，见 net/http/transport.go
	if err != nil {
		stopTimer()
		if resp != nil {
			log.Printf("RoundTripper returned a response & error; ignoring response")
		}
		if tlsErr, ok := err.(tls.RecordHeaderError); ok {
			// If we get a bad TLS record header, check to see if the
			// response looks like HTTP and give a more helpful error.
			// See golang.org/issue/11111.
			if string(tlsErr.RecordHeader[:]) == "HTTP/" {
				err = errors.New("http: server gave HTTP response to HTTPS client")
			}
		}
		return nil, didTimeout, err
	}
	if resp == nil {
		return nil, didTimeout, fmt.Errorf("http: RoundTripper implementation (%T) returned a nil *Response with a nil error", rt)
	}
	if resp.Body == nil {
		// The documentation on the Body field says “The http Client and Transport
		// guarantee that Body is always non-nil, even on responses without a body
		// or responses with a zero-length body.” Unfortunately, we didn't document
		// that same constraint for arbitrary RoundTripper implementations, and
		// RoundTripper implementations in the wild (mostly in tests) assume that
		// they can use a nil Body to mean an empty one (similar to Request.Body).
		// (See https://golang.org/issue/38095.)
		//
		// If the ContentLength allows the Body to be empty, fill in an empty one
		// here to ensure that it is non-nil.
		if resp.ContentLength > 0 && req.Method != "HEAD" {
			return nil, didTimeout, fmt.Errorf("http: RoundTripper implementation (%T) returned a *Response with content length %d but a nil Body", rt, resp.ContentLength)
		}
		resp.Body = io.NopCloser(strings.NewReader(""))
	}
	if !deadline.IsZero() {
		resp.Body = &cancelTimerBody{
			stop:          stopTimer,
			rc:            resp.Body,
			reqDidTimeout: didTimeout,
		}
	}
	return resp, nil, nil
}

// timeBeforeContextDeadline reports whether the non-zero Time t is
// before ctx's deadline, if any. If ctx does not have a deadline, it
// always reports true (the deadline is considered infinite).
func timeBeforeContextDeadline(t time.Time, ctx context.Context) bool {
	d, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return t.Before(d)
}

// knownRoundTripperImpl reports whether rt is a RoundTripper that's
// maintained by the Go team and known to implement the latest
// optional semantics (notably contexts). The Request is used
// to check whether this particular request is using an alternate protocol,
// in which case we need to check the RoundTripper for that protocol.
func knownRoundTripperImpl(rt RoundTripper, req *Request) bool {
	switch t := rt.(type) {
	case *Transport:
		if altRT := t.alternateRoundTripper(req); altRT != nil {
			return knownRoundTripperImpl(altRT, req)
		}
		return true
	case *http2Transport, http2noDialH2RoundTripper:
		return true
	}
	// There's a very minor chance of a false positive with this.
	// Instead of detecting our golang.org/x/net/http2.Transport,
	// it might detect a Transport type in a different http2
	// package. But I know of none, and the only problem would be
	// some temporarily leaked goroutines if the transport didn't
	// support contexts. So this is a good enough heuristic:
	if reflect.TypeOf(rt).String() == "*http2.Transport" {
		return true
	}
	return false
}

// 设置 req cancel 函数，在超时时间达到后取消请求，由于默认的 client 超时时间为 0，因此不会触发 select 中的 case，使得请求永远不会停止
func setRequestCancel(req *Request, rt RoundTripper, deadline time.Time) (stopTimer func(), didTimeout func() bool) {
	if deadline.IsZero() {
		return nop, alwaysFalse
	}
	knownTransport := knownRoundTripperImpl(rt, req)
	oldCtx := req.Context()

	if req.Cancel == nil && knownTransport {
		// If they already had a Request.Context that's
		// expiring sooner, do nothing:
		if !timeBeforeContextDeadline(deadline, oldCtx) {
			return nop, alwaysFalse
		}

		var cancelCtx func()
		req.ctx, cancelCtx = context.WithDeadline(oldCtx, deadline)
		return cancelCtx, func() bool { return time.Now().After(deadline) }
	}
	initialReqCancel := req.Cancel // the user's original Request.Cancel, if any

	var cancelCtx func()
	if oldCtx := req.Context(); timeBeforeContextDeadline(deadline, oldCtx) {
		req.ctx, cancelCtx = context.WithDeadline(oldCtx, deadline)
	}

	cancel := make(chan struct{})
	req.Cancel = cancel

	doCancel := func() {
		// The second way in the func comment above:
		close(cancel)
		// The first way, used only for RoundTripper
		// implementations written before Go 1.5 or Go 1.6.
		type canceler interface{ CancelRequest(*Request) }
		if v, ok := rt.(canceler); ok {
			v.CancelRequest(req)
		}
	}

	stopTimerCh := make(chan struct{})
	var once sync.Once
	stopTimer = func() {
		once.Do(func() {
			close(stopTimerCh)
			if cancelCtx != nil {
				cancelCtx()
			}
		})
	}

	timer := time.NewTimer(time.Until(deadline))
	var timedOut atomicBool

	go func() {
		select {
		case <-initialReqCancel:
			doCancel()
			timer.Stop()
		case <-timer.C:
			timedOut.setTrue()
			doCancel()
		case <-stopTimerCh:
			timer.Stop()
		}
	}()

	return stopTimer, timedOut.isSet
}

// basic auth 编码
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

// 使用默认 client 发送 Get 请求，该方法会自动处理 301、302、303、307、308 响应，最多重定向 10 次，自定义 Header 需要使用 NewRequest 创建 Req，然后调用 Client 的 Do 方法发送请求
// 如果要使用 Context，则使用 NewRequestWithContext 创建一个 Req，然后调用 Client 的 Do 方法发送请求
func Get(url string) (resp *Response, err error) {
	return DefaultClient.Get(url)
}

//使用自定义的 client 发送 Get 请求，首先调用 NewRequest 创建一个 Request， 然后发送请求
func (c *Client) Get(url string) (resp *Response, err error) {
	req, err := NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

func alwaysFalse() bool { return false }

// ErrUseLastResponse can be returned by Client.CheckRedirect hooks to
// control how redirects are processed. If returned, the next request
// is not sent and the most recent response is returned with its body
// unclosed.
var ErrUseLastResponse = errors.New("net/http: use last response")

// 执行重定向函数，默认使用 defaultCheckRedirect 重定向函数
func (c *Client) checkRedirect(req *Request, via []*Request) error {
	fn := c.CheckRedirect
	if fn == nil {
		fn = defaultCheckRedirect
	}
	return fn(req, via)
}

// redirectBehavior describes what should happen when the
// client encounters a 3xx status code from the server
// 处理响应状态码为 3xx 的响应
func redirectBehavior(reqMethod string, resp *Response, ireq *Request) (redirectMethod string, shouldRedirect, includeBody bool) {
	switch resp.StatusCode {
	case 301, 302, 303:
		redirectMethod = reqMethod
		shouldRedirect = true
		includeBody = false

		// RFC 2616 allowed automatic redirection only with GET and
		// HEAD requests. RFC 7231 lifts this restriction, but we still
		// restrict other methods to GET to maintain compatibility.
		// See Issue 18570.
		if reqMethod != "GET" && reqMethod != "HEAD" {
			redirectMethod = "GET"
		}
	case 307, 308:
		redirectMethod = reqMethod
		shouldRedirect = true
		includeBody = true

		// Treat 307 and 308 specially, since they're new in
		// Go 1.8, and they also require re-sending the request body.
		if resp.Header.Get("Location") == "" {
			// 308s have been observed in the wild being served
			// without Location headers. Since Go 1.7 and earlier
			// didn't follow these codes, just stop here instead
			// of returning an error.
			// See Issue 17773.
			shouldRedirect = false
			break
		}
		if ireq.GetBody == nil && ireq.outgoingLength() != 0 {
			// We had a request body, and 307/308 require
			// re-sending it, but GetBody is not defined. So just
			// return this response to the user instead of an
			// error, like we did in Go 1.7 and earlier.
			shouldRedirect = false
		}
	}
	return redirectMethod, shouldRedirect, includeBody
}

// urlErrorOp returns the (*url.Error).Op value to use for the
// provided (*Request).Method value.
func urlErrorOp(method string) string {
	if method == "" {
		return "Get"
	}
	if lowerMethod, ok := ascii.ToLower(method); ok {
		return method[:1] + lowerMethod[1:]
	}
	return method
}

// Do sends an HTTP request and returns an HTTP response, following
// policy (such as redirects, cookies, auth) as configured on the
// client.
//
// An error is returned if caused by client policy (such as
// CheckRedirect), or failure to speak HTTP (such as a network
// connectivity problem). A non-2xx status code doesn't cause an
// error.
//
// If the returned error is nil, the Response will contain a non-nil
// Body which the user is expected to close. If the Body is not both
// read to EOF and closed, the Client's underlying RoundTripper
// (typically Transport) may not be able to re-use a persistent TCP
// connection to the server for a subsequent "keep-alive" request.
//
// The request Body, if non-nil, will be closed by the underlying
// Transport, even on errors.
//
// On error, any Response can be ignored. A non-nil Response with a
// non-nil error only occurs when CheckRedirect fails, and even then
// the returned Response.Body is already closed.
//
// Generally Get, Post, or PostForm will be used instead of Do.
//
// If the server replies with a redirect, the Client first uses the
// CheckRedirect function to determine whether the redirect should be
// followed. If permitted, a 301, 302, or 303 redirect causes
// subsequent requests to use HTTP method GET
// (or HEAD if the original request was HEAD), with no body.
// A 307 or 308 redirect preserves the original HTTP method and body,
// provided that the Request.GetBody function is defined.
// The NewRequest function automatically sets GetBody for common
// standard library body types.
//
// Any returned error will be of type *url.Error. The url.Error
// value's Timeout method will report true if the request timed out.

// client 暴露的发送请求方法
func (c *Client) Do(req *Request) (*Response, error) {
	return c.do(req)
}

var testHookClientDoResult func(retres *Response, reterr error)

// 执行 http 请求发送
func (c *Client) do(req *Request) (retres *Response, reterr error) {
	if testHookClientDoResult != nil {
		defer func() { testHookClientDoResult(retres, reterr) }()
	}
	if req.URL == nil {
		req.closeBody()
		return nil, &url.Error{
			Op:  urlErrorOp(req.Method),
			Err: errors.New("http: nil Request.URL"),
		}
	}

	var (
		deadline      = c.deadline()
		reqs          []*Request
		resp          *Response
		copyHeaders   = c.makeHeadersCopier(req)
		reqBodyClosed = false // have we closed the current req.Body?

		// Redirect behavior:
		redirectMethod string
		includeBody    bool
	)
	uerr := func(err error) error {
		// the body may have been closed already by c.send()
		if !reqBodyClosed {
			req.closeBody()
		}
		var urlStr string
		if resp != nil && resp.Request != nil {
			urlStr = stripPassword(resp.Request.URL)
		} else {
			urlStr = stripPassword(req.URL)
		}
		return &url.Error{
			Op:  urlErrorOp(reqs[0].Method),
			URL: urlStr,
			Err: err,
		}
	}
	for {
		// For all but the first request, create the next
		// request hop and replace req.
		if len(reqs) > 0 {
			loc := resp.Header.Get("Location")
			if loc == "" {
				resp.closeBody()
				return nil, uerr(fmt.Errorf("%d response missing Location header", resp.StatusCode))
			}
			u, err := req.URL.Parse(loc)
			if err != nil {
				resp.closeBody()
				return nil, uerr(fmt.Errorf("failed to parse Location header %q: %v", loc, err))
			}
			host := ""
			if req.Host != "" && req.Host != req.URL.Host {
				//  See issue #22233.
				// 当用户设置特定 Host 头，且重定地址为一个相对地址时，重定向请求使用用户设置的 host， 没看出校验了是否时相对地址？
				if u, _ := url.Parse(loc); u != nil && !u.IsAbs() {
					host = req.Host
				}
			}
			ireq := reqs[0]
			req = &Request{
				Method:   redirectMethod,
				Response: resp,
				URL:      u,
				Header:   make(Header),
				Host:     host,
				Cancel:   ireq.Cancel,
				ctx:      ireq.ctx,
			}
			if includeBody && ireq.GetBody != nil { // 重定向请求不发送 body
				req.Body, err = ireq.GetBody()
				if err != nil {
					resp.closeBody()
					return nil, uerr(err)
				}
				req.ContentLength = ireq.ContentLength
			}
			copyHeaders(req)

			// Add the Referer header from the most recent
			// request URL to the new one, if it's not https->http:
			if ref := refererForURL(reqs[len(reqs)-1].URL, req.URL); ref != "" {
				req.Header.Set("Referer", ref)
			}
			err = c.checkRedirect(req, reqs)

			// Sentinel error to let users select the
			// previous response, without closing its
			// body. See Issue 10069.
			if err == ErrUseLastResponse {
				return resp, nil
			}

			// Close the previous response's body. But
			// read at least some of the body so if it's
			// small the underlying TCP connection will be
			// re-used. No need to check for errors: if it
			// fails, the Transport won't reuse it anyway.
			const maxBodySlurpSize = 2 << 10
			if resp.ContentLength == -1 || resp.ContentLength <= maxBodySlurpSize {
				io.CopyN(io.Discard, resp.Body, maxBodySlurpSize)
			}
			resp.Body.Close()

			if err != nil {
				// Special case for Go 1 compatibility: return both the response
				// and an error if the CheckRedirect function failed.
				// See https://golang.org/issue/3795
				// The resp.Body has already been closed.
				ue := uerr(err)
				ue.(*url.Error).URL = loc
				return resp, ue
			}
		}

		reqs = append(reqs, req)
		var err error
		var didTimeout func() bool
		if resp, didTimeout, err = c.send(req, deadline); err != nil { // 调用 send 方法发送请求
			// c.send() always closes req.Body
			reqBodyClosed = true
			if !deadline.IsZero() && didTimeout() {
				err = &httpError{
					err:     err.Error() + " (Client.Timeout exceeded while awaiting headers)",
					timeout: true,
				}
			}
			return nil, uerr(err)
		}

		var shouldRedirect bool
		redirectMethod, shouldRedirect, includeBody = redirectBehavior(req.Method, resp, reqs[0]) // 调用 redirectBehavior 函数判断是否进行重定向
		if !shouldRedirect {
			return resp, nil
		}

		req.closeBody()
	}
}

// 在发生重定向时，复制请求头以便重定向请求使用， 该方法使用闭包来保持对原始头数据的引用
func (c *Client) makeHeadersCopier(ireq *Request) func(*Request) {
	var (
		ireqhdr  = cloneOrMakeHeader(ireq.Header)
		icookies map[string][]*Cookie
	)
	if c.Jar != nil && ireq.Header.Get("Cookie") != "" {
		icookies = make(map[string][]*Cookie)
		for _, c := range ireq.Cookies() {
			icookies[c.Name] = append(icookies[c.Name], c)
		}
	}

	preq := ireq // The previous request
	return func(req *Request) {
		// If Jar is present and there was some initial cookies provided
		// via the request header, then we may need to alter the initial
		// cookies as we follow redirects since each redirect may end up
		// modifying a pre-existing cookie.
		//
		// Since cookies already set in the request header do not contain
		// information about the original domain and path, the logic below
		// assumes any new set cookies override the original cookie
		// regardless of domain or path.
		//
		// See https://golang.org/issue/17494
		if c.Jar != nil && icookies != nil {
			var changed bool
			resp := req.Response // The response that caused the upcoming redirect
			for _, c := range resp.Cookies() {
				if _, ok := icookies[c.Name]; ok {
					delete(icookies, c.Name)
					changed = true
				}
			}
			if changed {
				ireqhdr.Del("Cookie")
				var ss []string
				for _, cs := range icookies {
					for _, c := range cs {
						ss = append(ss, c.Name+"="+c.Value)
					}
				}
				sort.Strings(ss) // Ensure deterministic headers
				ireqhdr.Set("Cookie", strings.Join(ss, "; "))
			}
		}

		// Copy the initial request's Header values
		// (at least the safe ones).
		for k, vv := range ireqhdr {
			if shouldCopyHeaderOnRedirect(k, preq.URL, req.URL) {
				req.Header[k] = vv
			}
		}

		preq = req // Update previous Request with the current request
	}
}

// 默认重定向校验函数，主要功能为统计重定向次数，超过十次返回错误
func defaultCheckRedirect(req *Request, via []*Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// If the provided body is an io.Closer, it is closed after the request.
// 使用默认 client 发送 post 请求，调用户需要主动关闭 resp.Body，自定义 request 需要使用 NewRequest 创建 request，然后使用 client 的 Do 方法发送 request
func Post(url, contentType string, body io.Reader) (resp *Response, err error) {
	return DefaultClient.Post(url, contentType, body)
}

// 自定义 Client 的 Post 方法，与 Get 一样，先创建 Request，然后调用 Do 方法发送 Reqeust
func (c *Client) Post(url, contentType string, body io.Reader) (resp *Response, err error) {
	req, err := NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}

// 使用默认的 client 以 form 形式发送 post 请求，参数会编码进 URL，该方法需要主动关闭 resp.Body
func PostForm(url string, data url.Values) (resp *Response, err error) {
	return DefaultClient.PostForm(url, data)
}

// 自定义 Client 的 Post 方法，，先创建 Request，然后调用 Post 方法发送 Reqeust，设置 content-type，与 post 方法的唯一区别是 content-type
func (c *Client) PostForm(url string, data url.Values) (resp *Response, err error) {
	return c.Post(url, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
}

// 使用默认的 client 发送 Head 请求，最多支持重定向(301、302、303、307、308) 10 次
func Head(url string) (resp *Response, err error) {
	return DefaultClient.Head(url)
}

// 使用自定义的 client 发送 Head 请求，最多支持重定向(301、302、303、307、308) 10 次
func (c *Client) Head(url string) (resp *Response, err error) {
	req, err := NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// 客户端的 Transport 包含 CloseIdleConnections 方法时，调用 CloseIdleConnections 方法关闭所有未使用的连接
func (c *Client) CloseIdleConnections() {
	type closeIdler interface {
		CloseIdleConnections()
	}
	if tr, ok := c.transport().(closeIdler); ok {
		tr.CloseIdleConnections()
	}
}

// cancelTimerBody is an io.ReadCloser that wraps rc with two features:
// 1) On Read error or close, the stop func is called.
// 2) On Read failure, if reqDidTimeout is true, the error is wrapped and
//    marked as net.Error that hit its timeout.
type cancelTimerBody struct {
	stop          func() // stops the time.Timer waiting to cancel the request
	rc            io.ReadCloser
	reqDidTimeout func() bool
}

func (b *cancelTimerBody) Read(p []byte) (n int, err error) {
	n, err = b.rc.Read(p)
	if err == nil {
		return n, nil
	}
	b.stop()
	if err == io.EOF {
		return n, err
	}
	if b.reqDidTimeout() {
		err = &httpError{
			err:     err.Error() + " (Client.Timeout or context cancellation while reading body)",
			timeout: true,
		}
	}
	return n, err
}

func (b *cancelTimerBody) Close() error {
	err := b.rc.Close()
	b.stop()
	return err
}

func shouldCopyHeaderOnRedirect(headerKey string, initial, dest *url.URL) bool {
	switch CanonicalHeaderKey(headerKey) {
	case "Authorization", "Www-Authenticate", "Cookie", "Cookie2":
		// Permit sending auth/cookie headers from "foo.com"
		// to "sub.foo.com".

		// Note that we don't send all cookies to subdomains
		// automatically. This function is only used for
		// Cookies set explicitly on the initial outgoing
		// client request. Cookies automatically added via the
		// CookieJar mechanism continue to follow each
		// cookie's scope as set by Set-Cookie. But for
		// outgoing requests with the Cookie header set
		// directly, we don't know their scope, so we assume
		// it's for *.domain.com.

		ihost := canonicalAddr(initial)
		dhost := canonicalAddr(dest)
		return isDomainOrSubdomain(dhost, ihost)
	}
	// All other headers are copied:
	return true
}

// isDomainOrSubdomain reports whether sub is a subdomain (or exact
// match) of the parent domain.
//
// Both domains must already be in canonical form.
func isDomainOrSubdomain(sub, parent string) bool {
	if sub == parent {
		return true
	}
	// If sub is "foo.example.com" and parent is "example.com",
	// that means sub must end in "."+parent.
	// Do it without allocating.
	if !strings.HasSuffix(sub, parent) {
		return false
	}
	return sub[len(sub)-len(parent)-1] == '.'
}

func stripPassword(u *url.URL) string {
	_, passSet := u.User.Password()
	if passSet {
		return strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
	}
	return u.String()
}
