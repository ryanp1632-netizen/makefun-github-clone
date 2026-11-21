// The HTTP front door: /healthz, git smart HTTP, REST v3, GraphQL and MCP, with bearer-token auth
// and GitHub's JSON reply discipline.

package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

var TOKEN = os.Getenv("MAKEFUN_SHOP_TOKEN")

const jsonType = "application/json; charset=utf-8"

var requestCounter int

func requestID() string {
	requestCounter++
	return "F0RG:" + strings.ToUpper(strconv.FormatInt(int64(requestCounter), 16))
}

func jsonHeaders(extra []KV) []KV {
	h := make([]KV, 0, 2+len(extra))
	h = append(h, KV{"content-type", jsonType}, KV{"x-github-request-id", requestID()})
	for _, kv := range extra {
		set := false
		for i := range h {
			if h[i].K == kv.K {
				h[i].V = kv.V
				set = true
			}
		}
		if !set {
			h = append(h, kv)
		}
	}
	return h
}

func sendText(status int, text []byte, extra []KV) *Resp {
	return &Resp{Status: status, Headers: jsonHeaders(extra), Body: text}
}

func sendJSON(status int, body any, extra []KV) *Resp {
	b := appendJSON(make([]byte, 0, 512), body)
	return sendText(status, append(b, '\n'), extra)
}

func errorBody(status int, message string, extra O) O {
	o := O{{"message", message}, {"documentation_url", "https://docs.github.com/rest"}, {"status", strconv.Itoa(status)}}
	for _, kv := range extra {
		o = o.Set(kv.K, kv.V)
	}
	return o
}

func sendError(status int, message string, extra O) *Resp {
	return sendJSON(status, errorBody(status, message, extra), nil)
}

var authRe = regexp.MustCompile(`(?i)^(?:Bearer|token)\s+(\S+)\s*$`)
var basicRe = regexp.MustCompile(`(?i)^Basic\s+`)

// auth returns "ok", "missing" or "bad" for the request's credentials.
func auth(req *Req) string {
	h, ok := req.Headers["authorization"]
	if !ok || h == "" {
		return "missing"
	}
	token := ""
	found := false
	if strings.HasPrefix(h, "Bearer ") {
		token, found = strings.TrimSpace(h[7:]), true
	} else if m := authRe.FindStringSubmatch(h); m != nil {
		token, found = m[1], true
	} else if basicRe.MatchString(h) {
		decoded := toValidUTF8(lenientB64(basicRe.ReplaceAllString(h, "")))
		token, found = decoded[strings.IndexByte(decoded, ':')+1:], true
	}
	if !found || token == "" {
		return "bad"
	}
	if TOKEN != "" && token != TOKEN {
		return "bad"
	}
	return "ok"
}

func decodeURIComponent(s string) (string, bool) {
	if strings.IndexByte(s, '%') < 0 {
		return s, true
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", false
			}
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return string(b), true
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func mustDecode(s string) string {
	d, ok := decodeURIComponent(s)
	if !ok {
		panic("URIError: URI malformed")
	}
	return d
}

// gitPath matches /owner/name.git/(info/refs|git-upload-pack|git-receive-pack).
func gitPath(path string) (owner, name, service string, ok bool) {
	var rest string
	switch {
	case strings.HasSuffix(path, ".git/info/refs"):
		service, rest = "info/refs", path[:len(path)-len("/info/refs")]
	case strings.HasSuffix(path, ".git/git-upload-pack"):
		service, rest = "git-upload-pack", path[:len(path)-len("/git-upload-pack")]
	case strings.HasSuffix(path, ".git/git-receive-pack"):
		service, rest = "git-receive-pack", path[:len(path)-len("/git-receive-pack")]
	default:
		return
	}
	// rest = /owner/name.git
	if len(rest) < 2 || rest[0] != '/' {
		return
	}
	parts := strings.Split(rest[1:], "/")
	if len(parts) != 2 || parts[0] == "" {
		return
	}
	seg := parts[1]
	if len(seg) <= 4 {
		return
	}
	return parts[0], seg[:len(seg)-4], service, true
}

func handle(req *Req) *Resp {
	method := req.Method
	rawURL := req.URL
	path, search := rawURL, ""
	if q := strings.IndexByte(rawURL, '?'); q >= 0 {
		path, search = rawURL[:q], rawURL[q+1:]
	}

	if path == "/healthz" {
		return sendJSON(200, O{{"ok", true}}, nil)
	}

	isAPI := strings.HasPrefix(path, "/api/v3")
	if !isAPI {
		if o, n, svc, ok := gitPath(path); ok {
			owner := mustDecode(o)
			name := mustDecode(n)
			if auth(req) == "bad" {
				return &Resp{Status: 401, Headers: []KV{{"content-type", jsonType}, {"www-authenticate", "Basic realm=\"forge\""}}, Body: []byte("{\"message\":\"Bad credentials\"}\n")}
			}
			if svc == "info/refs" && method == "GET" {
				return handleInfoRefs(req, owner, name, parseQuery(search).get("service"))
			}
			if method == "POST" && svc == "git-upload-pack" {
				return handleUploadPack(req, owner, name, req.Body)
			}
			if method == "POST" && svc == "git-receive-pack" {
				return handleReceivePack(owner, name, req.Body)
			}
			return sendError(405, "Method Not Allowed", nil)
		}
	}

	isRestAPI := isAPI && (len(path) == 7 || path[7] == '/')
	isGraphql := path == "/api/graphql" || path == "/graphql" || path == "/api/graphql/"
	isMcp := path == "/mcp" || path == "/mcp/"

	if !isRestAPI && !isGraphql && !isMcp {
		if method == "GET" && path == "/" {
			return sendJSON(200, O{{"message", "forge"}, {"api", "/api/v3"}, {"graphql", "/api/graphql"}, {"mcp", "/mcp"}}, nil)
		}
		return sendError(404, "Not Found", nil)
	}

	if a := auth(req); a != "ok" {
		if isMcp {
			body := toJSON(O{{"jsonrpc", "2.0"}, {"id", nil}, {"error", O{{"code", -32001}, {"message", "Unauthorized: a bearer token is required"}}}})
			return &Resp{Status: 401, Headers: []KV{{"content-type", jsonType}, {"www-authenticate", "Bearer realm=\"forge\""}, {"x-github-request-id", requestID()}}, Body: append(body, '\n')}
		}
		if a == "missing" {
			return sendError(401, "Requires authentication", nil)
		}
		return sendError(401, "Bad credentials", nil)
	}

	body := req.Body
	host, ok := req.Headers["host"]
	if !ok {
		host = "localhost"
	}
	origin := "http://" + host

	// Hot path: GET one issue answers a cached, fully framed reply.
	if method == "GET" && isRestAPI {
		if res := fastIssue(path, origin); res != nil {
			return res
		}
	}

	if isMcp {
		switch method {
		case "GET":
			return sendError(405, "Method Not Allowed: this server does not stream on GET", nil)
		case "DELETE":
			return &Resp{Status: 204, Headers: []KV{{"x-github-request-id", requestID()}}}
		case "POST":
		default:
			return sendError(405, "Method Not Allowed", nil)
		}
		status, out := handleMcp(body, origin)
		if out == nil {
			return &Resp{Status: 202, Headers: []KV{{"x-github-request-id", requestID()}}}
		}
		return sendJSON(status, out, nil)
	}

	if isGraphql {
		if method != "POST" {
			return sendError(405, "Method Not Allowed: GraphQL takes POST", nil)
		}
		parsed, err := parseJSON(body)
		if err != nil {
			return sendError(400, "Problems parsing JSON", nil)
		}
		if arr, ok := parsed.([]any); ok {
			out := make([]any, len(arr))
			for i, op := range arr {
				out[i] = executeGraphql(op, origin)
			}
			return sendJSON(200, out, nil)
		}
		return sendJSON(200, executeGraphql(parsed, origin), nil)
	}

	// REST v3
	var parsedBody any
	hasBody := false
	if len(body) > 0 {
		v, err := parseJSON(body)
		if err != nil {
			return sendError(400, "Problems parsing JSON", nil)
		}
		parsedBody, hasBody = v, true
	}
	m := method
	if m == "HEAD" {
		m = "GET"
	}
	sub := "/"
	if len(path) > 7 {
		sub = path[7:]
	}
	ctx := &Ctx{Method: m, Path: sub, Query: parseQuery(search), Body: parsedBody, HasBody: hasBody, Origin: origin}
	reply, herr := dispatch(ctx)
	if herr != nil {
		return sendError(herr.Status, herr.Message, herr.Extra)
	}
	if reply.Status == 204 {
		h := jsonHeaders(nil)
		return &Resp{Status: 204, Headers: h[1:]}
	}
	if reply.Raw != nil {
		return sendText(reply.Status, reply.Raw, reply.Headers)
	}
	return sendJSON(reply.Status, reply.Body, reply.Headers)
}

// fastIssue serves GET /api/v3/repos/{o}/{r}/issues/{n} from a cached whole reply.
func fastIssue(path, origin string) *Resp {
	const prefix = "/api/v3/repos/"
	if !strings.HasPrefix(path, prefix) {
		return nil
	}
	parts := strings.Split(path[len(prefix):], "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "issues" || parts[3] == "" {
		return nil
	}
	for i := 0; i < len(parts[3]); i++ {
		if parts[3][i] < '0' || parts[3][i] > '9' {
			return nil
		}
	}
	o, ok1 := decodeURIComponent(parts[0])
	n, ok2 := decodeURIComponent(parts[1])
	if !ok1 || !ok2 {
		return nil
	}
	r := getRepo(o, n)
	if r == nil || strings.ToLower(r.Owner) != strings.ToLower(o) {
		return nil
	}
	num, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return nil
	}
	i := r.ByNumber[int(num)]
	if i == nil || float64(int(num)) != num {
		return nil
	}
	js := issueJSON(i, origin)
	if i.wire == nil || &i.wireJSON[0] != &js[0] {
		i.wire = wireResponse(200, []KV{{"content-type", jsonType}, {"x-github-request-id", "F0RG:I" + strings.ToUpper(strconv.FormatInt(int64(i.ID), 16))}}, js)
		i.wireJSON = js
	}
	return &Resp{Raw: i.wire}
}
