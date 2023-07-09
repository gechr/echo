package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	srvReadTimeout       = 5 * time.Second
	srvReadHeaderTimeout = 1 * time.Second
	srvMaxHeaderBytes    = 16 * 1024 // 16kb

	headerEchoHost   = "X-Nginx-Echo-Host"
	headerEchoIP     = "X-Nginx-Echo-Ip"
	headerEchoScheme = "X-Nginx-Echo-Scheme"
)

type responseWithoutBody struct {
	Origin  string         `json:"origin"`
	Method  string         `json:"method"`
	Headers map[string]any `json:"headers"`
	URL     string         `json:"url"`
	Params  map[string]any `json:"params,omitempty"`
}

type responseWithBody struct {
	Origin  string         `json:"origin"`
	Method  string         `json:"method"`
	Headers map[string]any `json:"headers"`
	URL     string         `json:"url"`
	Params  map[string]any `json:"params,omitempty"`
	Data    string         `json:"data,omitempty"`
	JSON    any            `json:"json,omitempty"`
}

type responseError struct {
	Code   int    `json:"code"`
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

type headResponseWriter struct {
	w      http.ResponseWriter
	status int
	size   int64
}

func (hw *headResponseWriter) Write(b []byte) (int, error) {
	size, err := hw.w.Write(b)
	hw.size += int64(size)
	return size, err
}

func (hw *headResponseWriter) WriteHeader(s int) {
	hw.w.WriteHeader(s)
	hw.status = s
}

func (hw *headResponseWriter) Flush() {
	f := hw.w.(http.Flusher)
	f.Flush()
}

func (hw *headResponseWriter) Header() http.Header {
	return hw.w.Header()
}

func (hw *headResponseWriter) Status() int {
	if hw.status == 0 {
		return http.StatusOK
	}
	return hw.status
}

func (hw *headResponseWriter) Size() int64 {
	return hw.size
}

func configureHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", handleEcho)

	var handler http.Handler
	handler = mux
	handler = limitRequestSize(handler)
	handler = autohead(handler)

	return handler
}

func limitRequestSize(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1048576)
		}
		h.ServeHTTP(w, r)
	})
}

func autohead(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w = &headResponseWriter{w: w}
		}
		h.ServeHTTP(w, r)
	})
}

func cleanHeaders(headers http.Header) map[string]any {
	cleaned := map[string]any{}
	for k, v := range headers {
		if k == headerEchoHost || k == headerEchoIP || k == headerEchoScheme {
			continue
		}
		if len(v) == 1 {
			cleaned[k] = v[0]
		} else {
			cleaned[k] = v
		}
	}
	return cleaned
}

func encodeData(body []byte, contentType string) string {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	data := base64.URLEncoding.EncodeToString(body)
	return string("data:" + contentType + ";base64," + data)
}

func parseBody(w http.ResponseWriter, r *http.Request, resp *responseWithBody) error {
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}

	r.Body = io.NopCloser(bytes.NewBuffer(body))

	if len(body) == 0 {
		return nil
	}

	contentType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")

	switch contentType {
	case "text/html", "text/plain":
		return nil

	case "application/x-www-form-urlencoded":
		if r.Method == http.MethodDelete || r.Method == http.MethodGet {
			originalMethod := r.Method
			r.Method = http.MethodPost
			defer func() { r.Method = originalMethod }()
		}
		if err := r.ParseForm(); err != nil {
			return err
		}
		resp.Data = string(body)

	case "application/json":
		if err := json.NewDecoder(r.Body).Decode(&resp.JSON); err != nil {
			return err
		}

	default:
		resp.Data = encodeData(body, contentType)
	}

	return nil
}

func getHost(r *http.Request) string {
	return r.Header.Get(headerEchoHost)
}

func getHeaders(r *http.Request) http.Header {
	headers := r.Header.Clone()
	headers.Set("Host", getHost(r))
	if len(r.TransferEncoding) > 0 {
		headers.Set("Transfer-Encoding", strings.Join(r.TransferEncoding, ","))
	}
	return headers
}

func getIP(r *http.Request) string {
	return r.Header.Get(headerEchoIP)
}

func getScheme(r *http.Request) string {
	return r.Header.Get(headerEchoScheme)
}

func getParams(r *http.Request) map[string]any {
	params := map[string]any{}
	for k, v := range r.URL.Query() {
		if len(v) == 1 {
			params[k] = v[0]
		} else {
			params[k] = v
		}
	}
	return params
}

func getURL(r *http.Request) string {
	u := &url.URL{
		Scheme:     getScheme(r),
		Opaque:     r.URL.Opaque,
		User:       r.URL.User,
		Host:       getHost(r),
		RawPath:    r.URL.RawPath,
		ForceQuery: r.URL.ForceQuery,
		RawQuery:   r.URL.RawQuery,
		Fragment:   r.URL.Fragment,
	}

	if r.URL.Path != "/" {
		u.Path = r.URL.Path
	}

	return u.String()
}

func mustMarshalJSON(w io.Writer, val any) {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(val); err != nil {
		panic(err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, val any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	mustMarshalJSON(w, val)
}

func writeError(w http.ResponseWriter, code int, err error) {
	resp := responseError{
		Code:  code,
		Error: http.StatusText(code),
	}
	if err != nil {
		resp.Detail = err.Error()
	}
	writeJSON(w, code, resp)
}

func handleEchoWithoutBody(w http.ResponseWriter, r *http.Request) {
	ip, url, params, headers := getIP(r), getURL(r), getParams(r), getHeaders(r)

	resp := &responseWithoutBody{
		Headers: cleanHeaders(headers),
		Method:  r.Method,
		Origin:  ip,
		URL:     url,
	}

	if len(params) > 0 {
		resp.Params = params
	}

	writeJSON(w, http.StatusOK, resp)
}

func handleEchoWithBody(w http.ResponseWriter, r *http.Request) {
	ip, u, params, headers := getIP(r), getURL(r), getParams(r), getHeaders(r)

	resp := &responseWithBody{
		Headers: cleanHeaders(headers),
		Method:  r.Method,
		Origin:  ip,
		URL:     u,
	}

	if len(params) > 0 {
		resp.Params = params
	}

	if err := parseBody(w, r, resp); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func handleEcho(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete, http.MethodPatch, http.MethodPost, http.MethodPut:
		handleEchoWithBody(w, r)
	default:
		handleEchoWithoutBody(w, r)
	}
}

func main() {
	srv := &http.Server{
		Addr:              "127.0.0.1:7777",
		Handler:           configureHandler(),
		MaxHeaderBytes:    srvMaxHeaderBytes,
		ReadHeaderTimeout: srvReadHeaderTimeout,
		ReadTimeout:       srvReadTimeout,
	}

	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}
