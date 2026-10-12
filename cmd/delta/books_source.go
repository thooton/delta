package main

// The client for the book source's JSON API, ported from the standalone zlib
// CLI (itself a port of rookslog/zlibrary-mcp): same endpoints, form bodies,
// headers, cookies and domain selection. Nothing here may reach the agent
// unfiltered; errors go through booksError before they are shown.

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	booksUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	// Downloads go through a bare client that sends python-httpx's default
	// headers, as the reference does. It also offers br; we can't decode
	// brotli with the standard library, so we leave it out.
	httpxUserAgent      = "python-httpx/0.28.1"
	booksAcceptEncoding = "gzip, deflate"
	booksMaxRedirects   = 20
)

var (
	// Tried in order unless DELTA_BOOKS_DOMAIN pins one.
	booksDomains = []string{"z-library.ec", "z-library.sk", "1lib.sk"}
	// booksScheme is only changed by tests.
	booksScheme = "https"
	// Status codes served by domains behind the anti-bot wall.
	walledStatusCodes = map[int]bool{307: true, 403: true, 513: true, 517: true}
)

func setBooksHeaders(h http.Header) {
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Encoding", booksAcceptEncoding)
	h.Set("Connection", "keep-alive")
	h.Set("User-Agent", booksUserAgent)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
}

func setHTTPXHeaders(h http.Header) {
	h.Set("Accept", "*/*")
	h.Set("Accept-Encoding", booksAcceptEncoding)
	h.Set("Connection", "keep-alive")
	h.Set("User-Agent", httpxUserAgent)
}

// booksTransport speaks HTTP/1.1 only, like httpx's default.
func booksTransport(connectTimeout, readTimeout time.Duration) *http.Transport {
	var p http.Protocols
	p.SetHTTP1(true)
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
		Protocols:             &p,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: readTimeout,
		IdleConnTimeout:       5 * time.Second,
		MaxIdleConnsPerHost:   20,
	}
}

// cookieTransport attaches fixed cookies to every request, including redirect
// hops to other hosts, as the reference's domainless client cookies are.
type cookieTransport struct {
	base    http.RoundTripper
	cookies func() []*http.Cookie
}

func (t *cookieTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	present := map[string]bool{}
	for _, c := range r.Cookies() {
		present[c.Name] = true
	}
	for _, c := range t.cookies() {
		if !present[c.Name] {
			r.AddCookie(c)
		}
	}
	return t.base.RoundTrip(r)
}

func newBooksHTTPClient(cookies func() []*http.Cookie, connectTimeout, readTimeout time.Duration, follow bool) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar:       jar,
		Transport: &cookieTransport{base: booksTransport(connectTimeout, readTimeout), cookies: cookies},
	}
	if follow {
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= booksMaxRedirects {
				return fmt.Errorf("exceeded %d redirects", booksMaxRedirects)
			}
			return nil
		}
	} else {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

// decodedBody undoes Content-Encoding. We set Accept-Encoding ourselves, so
// net/http leaves this to us.
func decodedBody(resp *http.Response) (io.Reader, error) {
	var r io.Reader = resp.Body
	parts := strings.Split(resp.Header.Get("Content-Encoding"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		switch enc := strings.ToLower(strings.TrimSpace(parts[i])); enc {
		case "", "identity":
		case "gzip", "x-gzip":
			gz, err := gzip.NewReader(r)
			if err != nil {
				return nil, err
			}
			r = gz
		case "deflate":
			// zlib-wrapped per spec, but some servers send raw deflate.
			br := bufio.NewReader(r)
			if h, err := br.Peek(2); err == nil && h[0]&0x0f == 8 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0 {
				zr, err := zlib.NewReader(br)
				if err != nil {
					return nil, err
				}
				r = zr
			} else {
				r = flate.NewReader(br)
			}
		default:
			return nil, fmt.Errorf("unsupported content encoding %q", enc)
		}
	}
	return r, nil
}

func readBody(resp *http.Response) ([]byte, error) {
	r, err := decodedBody(resp)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

var errBooksWalled = errors.New("the book source is blocking requests from this machine right now; try again later")

// decodeBooksJSON returns the raw JSON body, classifying anti-bot walls.
func decodeBooksJSON(resp *http.Response) ([]byte, error) {
	body, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 200 && json.Valid(body) {
		return body, nil
	}
	if strings.Contains(strings.ToLower(string(body)), "diamwall") || walledStatusCodes[resp.StatusCode] {
		return nil, errBooksWalled
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("the book source returned HTTP %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("the book source returned something unexpected (HTTP %d)", resp.StatusCode)
}

// probeBooksDomain checks a domain serves real API JSON via the
// unauthenticated GET /eapi/info/domains (never login, which is rate-limited).
func probeBooksDomain(domain string) bool {
	client := newBooksHTTPClient(func() []*http.Cookie { return nil }, 8*time.Second, 8*time.Second, false)
	client.Timeout = 8 * time.Second
	req, err := http.NewRequest("GET", booksScheme+"://"+strings.TrimRight(strings.TrimSpace(domain), "/")+"/eapi/info/domains", nil)
	if err != nil {
		return false
	}
	setBooksHeaders(req.Header)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	body, err := readBody(resp)
	if err != nil || strings.Contains(strings.ToLower(string(body)), "diamwall") {
		return false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	_, ok := payload["domains"]
	return ok
}

func pinnedBooksDomain() string { return strings.TrimSpace(os.Getenv("DELTA_BOOKS_DOMAIN")) }

func resolveBooksDomain() string {
	if d := pinnedBooksDomain(); d != "" {
		return d
	}
	for _, d := range booksDomains {
		if probeBooksDomain(d) {
			return d
		}
	}
	return booksDomains[0]
}

// selectAdvertisedDomain picks the first usable entry from /eapi/info/domains,
// keeping current if it is listed before any healthy alternative.
func selectAdvertisedDomain(advertised []json.RawMessage, current string) string {
	for _, raw := range advertised {
		var domain string
		if json.Unmarshal(raw, &domain) != nil {
			var obj struct {
				Domain string `json:"domain"`
			}
			json.Unmarshal(raw, &obj)
			domain = obj.Domain
		}
		if domain == "" {
			continue
		}
		if domain == current || probeBooksDomain(domain) {
			return domain
		}
	}
	return ""
}

// flexString accepts JSON strings, numbers, booleans or null.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(b)))
	return nil
}

func (f flexString) ok() bool { return f == "1" || f == "true" }

type bookInfo struct {
	ID             flexString `json:"id"`
	Hash           flexString `json:"hash"`
	Title          flexString `json:"title"`
	Author         flexString `json:"author"`
	Year           flexString `json:"year"`
	Language       flexString `json:"language"`
	Extension      flexString `json:"extension"`
	Filesize       flexString `json:"filesize"`
	FilesizeString flexString `json:"filesizeString"`
	Publisher      flexString `json:"publisher"`
	Pages          flexString `json:"pages"`
}

type bookSearchResult struct {
	Success    flexString `json:"success"`
	Error      flexString `json:"error"`
	Books      []bookInfo `json:"books"`
	TotalPages flexString `json:"totalPages"`
	Pagination struct {
		TotalPages flexString `json:"total_pages"`
	} `json:"pagination"`
}

func (r *bookSearchResult) pages() int {
	n := 0
	fmt.Sscan(string(r.TotalPages), &n)
	if n == 0 {
		fmt.Sscan(string(r.Pagination.TotalPages), &n)
	}
	return max(n, 1)
}

type bookSearch struct {
	Query      string   `json:"query"`
	Limit      int      `json:"limit"`
	Page       int      `json:"page"`
	YearFrom   int      `json:"year_from,omitempty"` // 0 = unset
	YearTo     int      `json:"year_to,omitempty"`
	Languages  []string `json:"languages,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Exact      bool     `json:"exact,omitempty"`
	Order      string   `json:"order,omitempty"`
}

// booksForm is an insertion-ordered urlencoded body, matching httpx's encoding.
type booksForm [][2]string

func (f booksForm) encode() string {
	parts := make([]string, len(f))
	for i, kv := range f {
		parts[i] = url.QueryEscape(kv[0]) + "=" + url.QueryEscape(kv[1])
	}
	return strings.Join(parts, "&")
}

type booksClient struct {
	Domain  string
	UserID  string
	UserKey string
	http    *http.Client
}

func newBooksClient(domain, userID, userKey string) *booksClient {
	c := &booksClient{Domain: strings.TrimRight(domain, "/"), UserID: userID, UserKey: userKey}
	c.resetHTTP()
	return c
}

func (c *booksClient) resetHTTP() {
	c.http = newBooksHTTPClient(c.cookies, 10*time.Second, 30*time.Second, true)
	c.http.Timeout = 60 * time.Second
}

func (c *booksClient) cookies() []*http.Cookie {
	cs := []*http.Cookie{{Name: "siteLanguageV2", Value: "en"}}
	if c.UserID != "" {
		cs = append(cs, &http.Cookie{Name: "remix_userid", Value: c.UserID})
	}
	if c.UserKey != "" {
		cs = append(cs, &http.Cookie{Name: "remix_userkey", Value: c.UserKey})
	}
	return cs
}

func (c *booksClient) baseURL() string { return booksScheme + "://" + c.Domain }

func (c *booksClient) do(method, path string, body booksForm, out any) error {
	var rdr io.Reader
	if method == "POST" {
		rdr = strings.NewReader(body.encode())
	}
	req, err := http.NewRequest(method, c.baseURL()+path, rdr)
	if err != nil {
		return err
	}
	setBooksHeaders(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		return sourceError(err)
	}
	defer resp.Body.Close()
	raw, err := decodeBooksJSON(resp)
	if err != nil {
		return sourceError(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("the book source returned something unexpected")
	}
	return nil
}

// login posts to /eapi/user/login and keeps the returned session cookies.
func (c *booksClient) login(email, password string) error {
	var res struct {
		Success flexString `json:"success"`
		Error   flexString `json:"error"`
		User    *struct {
			ID           flexString `json:"id"`
			RemixUserKey flexString `json:"remix_userkey"`
		} `json:"user"`
	}
	if err := c.do("POST", "/eapi/user/login", booksForm{{"email", email}, {"password", password}}, &res); err != nil {
		return err
	}
	if !res.Success.ok() || res.User == nil {
		return fmt.Errorf("logging in to the book source failed: %s", firstNonEmpty(string(res.Error), "unknown error"))
	}
	c.UserID = string(res.User.ID)
	c.UserKey = string(res.User.RemixUserKey)
	c.resetHTTP()
	return nil
}

func (c *booksClient) domains() ([]json.RawMessage, error) {
	var res struct {
		Domains []json.RawMessage `json:"domains"`
	}
	err := c.do("GET", "/eapi/info/domains", nil, &res)
	return res.Domains, err
}

func (c *booksClient) search(p bookSearch) (*bookSearchResult, error) {
	f := booksForm{{"message", p.Query}, {"limit", fmt.Sprint(p.Limit)}, {"page", fmt.Sprint(p.Page)}}
	if p.YearFrom != 0 {
		f = append(f, [2]string{"yearFrom", fmt.Sprint(p.YearFrom)})
	}
	if p.YearTo != 0 {
		f = append(f, [2]string{"yearTo", fmt.Sprint(p.YearTo)})
	}
	for _, l := range p.Languages {
		f = append(f, [2]string{"languages[]", l})
	}
	for _, e := range p.Extensions {
		f = append(f, [2]string{"extensions[]", e})
	}
	if p.Exact {
		f = append(f, [2]string{"e", "1"})
	}
	if p.Order != "" {
		f = append(f, [2]string{"order", p.Order})
	}
	var res bookSearchResult
	if err := c.do("POST", "/eapi/book/search", f, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// downloadLink calls GET /eapi/book/{id}/{hash}/file.
func (c *booksClient) downloadLink(id, hash string) (string, error) {
	var res struct {
		Success      flexString `json:"success"`
		Error        flexString `json:"error"`
		DownloadLink string     `json:"downloadLink"`
		URL          string     `json:"url"`
		Link         string     `json:"link"`
		File         struct {
			DownloadLink string `json:"downloadLink"`
		} `json:"file"`
	}
	if err := c.do("GET", "/eapi/book/"+url.PathEscape(id)+"/"+url.PathEscape(hash)+"/file", nil, &res); err != nil {
		return "", err
	}
	link := firstNonEmpty(res.File.DownloadLink, res.DownloadLink, res.URL, res.Link)
	if link == "" {
		return "", errors.New(firstNonEmpty(string(res.Error), "no download link in the response"))
	}
	if strings.HasPrefix(link, "/") {
		link = c.baseURL() + link
	}
	return link, nil
}

// idleReader cancels the transfer when no data arrives for the read timeout.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	i.timer.Reset(i.d)
	return n, err
}

// openDownload streams the file at link through a bare client carrying only
// the session cookies.
func (c *booksClient) openDownload(link string) (resp *http.Response, body io.Reader, cancel func(), err error) {
	const readTimeout = 60 * time.Second
	ctx, cancelCtx := context.WithCancel(context.Background())
	client := newBooksHTTPClient(c.cookies, 10*time.Second, readTimeout, true)
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		cancelCtx()
		return nil, nil, nil, err
	}
	setHTTPXHeaders(req.Header)
	resp, err = client.Do(req)
	if err != nil {
		cancelCtx()
		return nil, nil, nil, sourceError(err)
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		cancelCtx()
		return nil, nil, nil, fmt.Errorf("the download failed (HTTP %d)", resp.StatusCode)
	}
	r, err := decodedBody(resp)
	if err != nil {
		resp.Body.Close()
		cancelCtx()
		return nil, nil, nil, err
	}
	timer := time.AfterFunc(readTimeout, cancelCtx)
	cancel = func() { timer.Stop(); resp.Body.Close(); cancelCtx() }
	return resp, &idleReader{r: r, timer: timer, d: readTimeout}, cancel, nil
}

// responseFilename is the Content-Disposition file name, else the last URL
// path segment. Only its extension is used.
func responseFilename(contentDisposition string, u *url.URL) string {
	if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
		if fn := params["filename"]; fn != "" {
			return fn
		}
	}
	if u != nil {
		return path.Base(u.Path)
	}
	return ""
}

var (
	errBooksUnreachable = errors.New("could not reach the book source; try again later")
	errBooksTimeout     = errors.New("the book source timed out; try again")
)

// sourceError replaces a network error, which carries the URL and host it
// failed on, with one that names neither.
func sourceError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if ue.Timeout() {
		return errBooksTimeout
	}
	return errBooksUnreachable
}

// isBooksNetError reports whether err is about reaching the source at all,
// rather than about the request.
func isBooksNetError(err error) bool {
	return errors.Is(err, errBooksUnreachable) || errors.Is(err, errBooksTimeout) || errors.Is(err, errBooksWalled)
}

// sourceNames matches anything that names the book source or its hosts.
var sourceNames = regexp.MustCompile(`(?i)(https?://)?[a-z0-9.-]*(z-?lib|zlibrary|1lib|singlelogin|diamwall)[a-z0-9./-]*`)

// booksError makes an error fit to show the agent: anything still naming the
// source is replaced.
func booksError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(sourceNames.ReplaceAllString(err.Error(), "the book source"))
}
