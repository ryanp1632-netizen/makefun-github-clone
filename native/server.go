// A lean HTTP/1.1 server on raw TCP: keep-alive and pipelining (answered in order, flushed once
// per batch), Content-Length and chunked bodies, Expect: 100-continue, Connection: close. Handlers
// run under one lock (the store is single-writer, like the Node event loop) and return the whole
// response as bytes, which is written after the lock is released.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"fmt"
)

// Req is one parsed request.
type Req struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// Resp is a response; Raw, when set, is a complete prebuilt keep-alive response.
type Resp struct {
	Status  int
	Headers []KV // values are strings
	Body    []byte
	Raw     []byte
}

var reasons = map[int]string{
	100: "Continue", 200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 400: "Bad Request",
	401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed", 409: "Conflict",
	413: "Payload Too Large", 422: "Unprocessable Entity", 500: "Internal Server Error",
}

func reason(status int) string {
	if r, ok := reasons[status]; ok {
		return r
	}
	return "Status"
}

// wireResponse builds a complete keep-alive response, for callers that cache whole replies.
func wireResponse(status int, headers []KV, body []byte) []byte {
	b := make([]byte, 0, 160+len(body))
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, reason(status)...)
	b = append(b, "\r\n"...)
	for _, h := range headers {
		b = append(b, h.K...)
		b = append(b, ": "...)
		b = append(b, h.V.(string)...)
		b = append(b, "\r\n"...)
	}
	b = append(b, "content-length: "...)
	b = strconv.AppendInt(b, int64(len(body)), 10)
	b = append(b, "\r\n\r\n"...)
	return append(b, body...)
}

// appendResponse writes res to out with the connection's framing.
func appendResponse(out []byte, res *Resp, closeConn, isHead bool) []byte {
	if res.Raw != nil {
		if !closeConn {
			if isHead {
				sep := bytes.Index(res.Raw, []byte("\r\n\r\n"))
				return append(out, res.Raw[:sep+4]...)
			}
			return append(out, res.Raw...)
		}
		sep := bytes.Index(res.Raw, []byte("\r\n\r\n"))
		out = append(out, res.Raw[:sep]...)
		out = append(out, "\r\nconnection: close\r\n\r\n"...)
		if !isHead {
			out = append(out, res.Raw[sep+4:]...)
		}
		return out
	}
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(res.Status), 10)
	out = append(out, ' ')
	out = append(out, reason(res.Status)...)
	out = append(out, "\r\n"...)
	hasLength := false
	for _, h := range res.Headers {
		lk := strings.ToLower(h.K)
		if lk == "content-length" {
			hasLength = true
			if res.Status == 204 {
				continue
			}
		}
		if lk == "connection" {
			continue
		}
		out = append(out, h.K...)
		out = append(out, ": "...)
		out = append(out, h.V.(string)...)
		out = append(out, "\r\n"...)
	}
	if !hasLength && res.Status != 204 {
		out = append(out, "content-length: "...)
		out = strconv.AppendInt(out, int64(len(res.Body)), 10)
		out = append(out, "\r\n"...)
	}
	if closeConn {
		out = append(out, "connection: close\r\n\r\n"...)
	} else {
		out = append(out, "\r\n"...)
	}
	if len(res.Body) > 0 && !isHead && res.Status != 204 {
		out = append(out, res.Body...)
	}
	return out
}

const badRequestWire = "HTTP/1.1 400 Bad Request\r\ncontent-type: application/json\r\ncontent-length: 27\r\nconnection: close\r\n\r\n{\"message\":\"Bad Request\"}\r\n"

const maxHeader = 64 * 1024
const maxBody = 512 * 1024 * 1024

var lock sync.Mutex

var errBad = errors.New("bad request")

func readHead(br *bufio.Reader) (method, url, proto string, headers map[string]string, err error) {
	total := 0
	line, err := readLine(br, &total)
	if err != nil {
		return
	}
	sp1 := strings.IndexByte(line, ' ')
	if sp1 <= 0 {
		err = errBad
		return
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 || !strings.HasPrefix(rest[sp2+1:], "HTTP/1.") {
		err = errBad
		return
	}
	method, url, proto = line[:sp1], rest[:sp2], rest[sp2+1:]
	headers = make(map[string]string, 8)
	for {
		var h string
		h, err = readLine(br, &total)
		if err != nil {
			return
		}
		if h == "" {
			return
		}
		c := strings.IndexByte(h, ':')
		if c > 0 {
			name := strings.ToLower(strings.TrimSpace(h[:c]))
			value := strings.TrimSpace(h[c+1:])
			if prev, ok := headers[name]; ok {
				headers[name] = prev + ", " + value
			} else {
				headers[name] = value
			}
		}
	}
}

func readLine(br *bufio.Reader, total *int) (string, error) {
	var acc []byte
	for {
		chunk, err := br.ReadSlice('\n')
		*total += len(chunk)
		if *total > maxHeader {
			return "", errBad
		}
		if err == bufio.ErrBufferFull {
			acc = append(acc, chunk...)
			continue
		}
		if err != nil {
			return "", err
		}
		if acc != nil {
			acc = append(acc, chunk...)
			chunk = acc
		}
		n := len(chunk) - 1
		if n > 0 && chunk[n-1] == '\r' {
			n--
		}
		return string(chunk[:n]), nil
	}
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var body []byte
	total := 0
	for {
		line, err := readLine(br, &total)
		total = 0
		if err != nil {
			return nil, err
		}
		if k := strings.IndexByte(line, ';'); k >= 0 {
			line = line[:k]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 || int64(len(body))+size > maxBody {
			return nil, errBad
		}
		if size == 0 {
			for {
				t, err := readLine(br, &total)
				total = 0
				if err != nil {
					return nil, err
				}
				if t == "" {
					return body, nil
				}
			}
		}
		start := len(body)
		body = append(body, make([]byte, size)...)
		if _, err := readFull(br, body[start:]); err != nil {
			return nil, err
		}
		var crlf [2]byte
		if _, err := readFull(br, crlf[:]); err != nil {
			return nil, err
		}
	}
}

func readFull(br *bufio.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := br.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func serveConn(c net.Conn, handler func(*Req) *Resp) {
	defer c.Close()
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "[forge] connection panicked: %v\n", p)
		}
	}()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(c, 16384)
	out := make([]byte, 0, 16384)
	for {
		method, url, proto, headers, err := readHead(br)
		if err != nil {
			if err == errBad {
				c.Write(append(out, badRequestWire...))
			} else if len(out) > 0 {
				c.Write(out)
			}
			return
		}
		var body []byte
		expect := strings.ToLower(headers["expect"]) == "100-continue"
		if strings.Contains(strings.ToLower(headers["transfer-encoding"]), "chunked") {
			if expect && br.Buffered() == 0 {
				out = append(out, "HTTP/1.1 100 Continue\r\n\r\n"...)
				c.Write(out)
				out = out[:0]
			}
			body, err = readChunked(br)
			if err != nil {
				c.Write(append(out, badRequestWire...))
				return
			}
		} else if cl, ok := headers["content-length"]; ok {
			n, perr := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
			if perr != nil || n < 0 || n > maxBody || strings.TrimSpace(cl) != cl && false {
				c.Write(append(out, badRequestWire...))
				return
			}
			if n > 0 {
				if expect && int64(br.Buffered()) < n {
					out = append(out, "HTTP/1.1 100 Continue\r\n\r\n"...)
					c.Write(out)
					out = out[:0]
				}
				body = make([]byte, n)
				if _, err := readFull(br, body); err != nil {
					return
				}
			}
		}
		closeConn := strings.ToLower(headers["connection"]) == "close" || proto == "HTTP/1.0"
		req := &Req{Method: method, URL: url, Headers: headers, Body: body}
		res := runHandler(handler, req)
		out = appendResponse(out, res, closeConn, method == "HEAD")
		if closeConn {
			c.Write(out)
			return
		}
		if br.Buffered() == 0 || len(out) > 1<<20 {
			if _, err := c.Write(out); err != nil {
				return
			}
			if cap(out) > 4<<20 {
				out = make([]byte, 0, 16384)
			} else {
				out = out[:0]
			}
		}
	}
}

func runHandler(handler func(*Req) *Resp, req *Req) (res *Resp) {
	lock.Lock()
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "[forge] handler panicked: %v\n%s\n", p, debug.Stack())
			res = &Resp{Status: 500, Headers: []KV{{"content-type", "application/json; charset=utf-8"}}, Body: []byte("{\"message\":\"Internal Server Error\"}\n")}
		}
		lock.Unlock()
	}()
	res = handler(req)
	if res == nil {
		res = &Resp{Status: 500, Headers: []KV{{"content-type", "application/json; charset=utf-8"}}, Body: []byte("{\"message\":\"no response\"}\n")}
	}
	return
}

func main() {
	// One P: every handler runs under the store lock anyway, and a single P parks in the netpoller
	// itself instead of waking another thread for each request. The trial's whole heap is small, so
	// collection only runs if it ever nears the memory limit.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(-1)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(192 << 20)
	}
	port := os.Getenv("FORGE_PORT")
	if port == "" {
		port = "0"
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "[forge] listening on port %d\n", ln.Addr().(*net.TCPAddr).Port)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go serveConn(c, handle)
	}
}
