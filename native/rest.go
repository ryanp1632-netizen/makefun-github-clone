// GitHub REST v3 under /api/v3: repositories, contents, refs, branches, commits, labels, issues,
// comments and pull requests, with GitHub's shapes, statuses and Link pagination.

package main

import (
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type HttpError struct {
	Status  int
	Message string
	Extra   O
}

func (e *HttpError) Error() string { return e.Message }

func herr(status int, msg string, extra ...KV) *HttpError {
	return &HttpError{Status: status, Message: msg, Extra: O(extra)}
}

// throw aborts the current handler with an HttpError (recovered by dispatch and the MCP/GraphQL callers).
func throw(status int, msg string, extra ...KV) { panic(herr(status, msg, extra...)) }

type Ctx struct {
	Method  string
	Path    string
	Query   *query
	Body    any
	HasBody bool
	Origin  string
}

type Reply struct {
	Status  int
	Body    any
	Raw     []byte
	Headers []KV
}

// ------------------------------------------------------------------ query strings (URLSearchParams)

type query struct{ kv [][2]string }

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	// URLSearchParams keeps malformed escapes as they are.
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return toValidUTF8([]byte(b.String()))
}

func parseQuery(s string) *query {
	q := &query{}
	if s == "" {
		return q
	}
	for _, part := range strings.Split(s, "&") {
		if part == "" {
			continue
		}
		k, v := part, ""
		if e := strings.IndexByte(part, '='); e >= 0 {
			k, v = part[:e], part[e+1:]
		}
		q.kv = append(q.kv, [2]string{formDecode(k), formDecode(v)})
	}
	return q
}

func (q *query) get(k string) string {
	v, _ := q.lookup(k)
	return v
}

func (q *query) lookup(k string) (string, bool) {
	for _, e := range q.kv {
		if e[0] == k {
			return e[1], true
		}
	}
	return "", false
}

func (q *query) clone() *query { return &query{kv: append([][2]string(nil), q.kv...)} }

func (q *query) set(k, v string) {
	idx := -1
	out := q.kv[:0:0]
	for _, e := range q.kv {
		if e[0] == k {
			if idx < 0 {
				idx = len(out)
				out = append(out, [2]string{k, v})
			}
			continue
		}
		out = append(out, e)
	}
	if idx < 0 {
		out = append(out, [2]string{k, v})
	}
	q.kv = out
}

func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

func (q *query) String() string {
	parts := make([]string, len(q.kv))
	for i, e := range q.kv {
		parts[i] = formEncode(e[0]) + "=" + formEncode(e[1])
	}
	return strings.Join(parts, "&")
}

func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("-_.!~*'()", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

// ------------------------------------------------------------------ views

func userView(origin string) O {
	return O{{"login", VIEWER.Login}, {"id", VIEWER.ID}, {"node_id", globalID("U", VIEWER.ID)}, {"type", "User"}, {"site_admin", false},
		{"name", VIEWER.Name}, {"email", VIEWER.Email}, {"url", origin + "/api/v3/users/" + VIEWER.Login}, {"html_url", origin + "/" + VIEWER.Login}, {"avatar_url", ""}}
}

func ownerView(origin string) O {
	return O{{"login", VIEWER.Login}, {"id", VIEWER.ID}, {"node_id", globalID("U", VIEWER.ID)}, {"type", "User"}, {"site_admin", false},
		{"url", origin + "/api/v3/users/" + VIEWER.Login}, {"html_url", origin + "/" + VIEWER.Login}, {"avatar_url", ""}}
}

func repoView(r *Repo, origin string) O {
	api := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	open := 0
	for _, i := range r.Issues {
		if i.State == "open" {
			open++
		}
	}
	vis := "public"
	if r.Private {
		vis = "private"
	}
	return O{
		{"id", r.ID}, {"node_id", globalID("R", r.ID)}, {"name", r.Name}, {"full_name", r.Owner + "/" + r.Name}, {"owner", ownerView(origin)},
		{"private", r.Private}, {"visibility", vis}, {"description", r.Description}, {"fork", false}, {"archived", false}, {"disabled", false},
		{"default_branch", r.DefaultBranch}, {"url", api}, {"html_url", origin + "/" + r.Owner + "/" + r.Name},
		{"clone_url", origin + "/" + r.Owner + "/" + r.Name + ".git"}, {"git_url", origin + "/" + r.Owner + "/" + r.Name + ".git"},
		{"issues_url", api + "/issues{/number}"}, {"pulls_url", api + "/pulls{/number}"},
		{"created_at", r.Created}, {"updated_at", r.Updated}, {"pushed_at", r.Pushed}, {"size", len(r.Objects)},
		{"has_issues", true}, {"has_projects", false}, {"has_wiki", false}, {"open_issues_count", open}, {"open_issues", open},
		{"permissions", O{{"admin", true}, {"maintain", true}, {"push", true}, {"triage", true}, {"pull", true}}},
	}
}

func labelView(l *Label, r *Repo, origin string) O {
	return O{{"id", l.ID}, {"node_id", globalID("LA", l.ID)}, {"url", origin + "/api/v3/repos/" + r.Owner + "/" + r.Name + "/labels/" + encodeURIComponent(l.Name)},
		{"name", l.Name}, {"color", l.Color}, {"description", l.Description}, {"default", false}}
}

func strPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func issueView(i *Issue, origin string) O {
	r := i.Repo
	api := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	kind := "issues"
	if i.Pull != nil {
		kind = "pull"
	}
	labels := make([]O, len(i.Labels))
	for k, l := range i.Labels {
		labels[k] = O{{"id", l.ID}, {"name", l.Name}, {"color", l.Color}, {"description", l.Description}}
	}
	v := O{
		{"id", i.ID}, {"node_id", globalID("I", i.ID)}, {"url", api + "/issues/" + strconv.Itoa(i.Number)},
		{"html_url", origin + "/" + r.Owner + "/" + r.Name + "/" + kind + "/" + strconv.Itoa(i.Number)},
		{"number", i.Number}, {"title", i.Title}, {"body", i.Body}, {"state", i.State},
		{"user", O{{"login", VIEWER.Login}, {"id", VIEWER.ID}, {"type", "User"}}},
		{"labels", labels}, {"comments", len(i.Comments)}, {"created_at", i.Created}, {"updated_at", i.Updated}, {"closed_at", strPtr(i.Closed)},
	}
	if i.Pull != nil {
		v = append(v, KV{"pull_request", O{{"url", api + "/pulls/" + strconv.Itoa(i.Number)}, {"merged_at", strPtr(i.Pull.MergedAt)}}})
	}
	return v
}

func issueJSON(i *Issue, origin string) []byte {
	if i.json == nil || i.jsonOrig != origin {
		i.json = toJSON(issueView(i, origin))
		i.jsonOrig = origin
	}
	return i.json
}

func commentView(c *Comment, origin string) O {
	r := c.Issue.Repo
	base := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	return O{{"id", c.ID}, {"node_id", globalID("IC", c.ID)}, {"url", base + "/issues/comments/" + strconv.Itoa(c.ID)},
		{"html_url", origin + "/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(c.Issue.Number) + "#issuecomment-" + strconv.Itoa(c.ID)},
		{"issue_url", base + "/issues/" + strconv.Itoa(c.Issue.Number)}, {"body", c.Body}, {"user", ownerView(origin)},
		{"created_at", c.Created}, {"updated_at", c.Created}, {"author_association", "OWNER"}}
}

func branchSide(r *Repo, ref, sha, origin string) O {
	return O{{"label", r.Owner + ":" + ref}, {"ref", ref}, {"sha", sha}, {"user", ownerView(origin)}, {"repo", repoView(r, origin)}}
}

func pullView(p *Pull, origin string) O {
	i := p.Issue
	r := i.Repo
	api := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	head, base := pullShas(p)
	var mergeable any
	mstate := "unknown"
	if i.State == "open" {
		m := isMergeable(p)
		mergeable = m
		if m {
			mstate = "clean"
		} else {
			mstate = "dirty"
		}
	}
	labels := make([]O, len(i.Labels))
	for k, l := range i.Labels {
		labels[k] = labelView(l, r, origin)
	}
	var mergedBy any
	if p.Merged {
		mergedBy = ownerView(origin)
	}
	n := strconv.Itoa(i.Number)
	return O{
		{"id", p.ID}, {"node_id", globalID("PR", p.ID)}, {"url", api + "/pulls/" + n}, {"html_url", origin + "/" + r.Owner + "/" + r.Name + "/pull/" + n},
		{"diff_url", origin + "/" + r.Owner + "/" + r.Name + "/pull/" + n + ".diff"}, {"issue_url", api + "/issues/" + n},
		{"number", i.Number}, {"state", i.State}, {"locked", false}, {"title", i.Title}, {"body", i.Body}, {"user", ownerView(origin)},
		{"labels", labels}, {"created_at", i.Created}, {"updated_at", i.Updated}, {"closed_at", strPtr(i.Closed)},
		{"merged_at", strPtr(p.MergedAt)}, {"merge_commit_sha", strPtr(p.MergeSha)}, {"draft", false},
		{"head", branchSide(r, p.Head, head, origin)}, {"base", branchSide(r, p.Base, base, origin)},
		{"merged", p.Merged}, {"mergeable", mergeable}, {"mergeable_state", mstate}, {"merged_by", mergedBy},
		{"comments", len(i.Comments)}, {"commits", 1}, {"author_association", "OWNER"},
	}
}

func identView(ident string) (O, string) {
	name, email, date := parseIdent(ident)
	return O{{"name", name}, {"email", email}, {"date", date}}, email
}

func commitView(r *Repo, sha, origin string) O {
	c := commitOf(r, sha)
	api := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	if c == nil {
		return O{{"sha", sha}}
	}
	a, ae := identView(c.Author)
	m, me := identView(c.Committer)
	n8 := sha
	if len(n8) > 8 {
		n8 = n8[:8]
	}
	nid, _ := strconv.ParseInt(n8, 16, 64)
	var author, committer any
	if ae == VIEWER.Email {
		author = ownerView(origin)
	}
	if me == VIEWER.Email {
		committer = ownerView(origin)
	}
	parents := make([]O, len(c.Parents))
	for k, p := range c.Parents {
		parents[k] = O{{"sha", p}, {"url", api + "/commits/" + p}}
	}
	return O{
		{"sha", sha}, {"node_id", globalID("C", int(nid))}, {"url", api + "/commits/" + sha}, {"html_url", origin + "/" + r.Owner + "/" + r.Name + "/commit/" + sha},
		{"commit", O{{"message", strings.TrimSuffix(c.Message, "\n")}, {"author", a}, {"committer", m},
			{"tree", O{{"sha", c.Tree}, {"url", api + "/git/trees/" + c.Tree}}}, {"url", api + "/git/commits/" + sha}}},
		{"author", author}, {"committer", committer}, {"parents", parents},
	}
}

// ------------------------------------------------------------------ helpers

func objBody(ctx *Ctx) map[string]any {
	if !ctx.HasBody || ctx.Body == nil {
		return map[string]any{}
	}
	m, ok := ctx.Body.(map[string]any)
	if !ok {
		throw(400, "Problems parsing JSON")
	}
	return m
}

func bstr(b map[string]any, k string) (string, bool) {
	s, ok := b[k].(string)
	return s, ok
}

func jsNumber(s string, present bool) float64 {
	if !present {
		return 0
	}
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func orDefault(f, d float64) float64 {
	if math.IsNaN(f) || f == 0 {
		return d
	}
	return f
}

func pageParams(q *query) (float64, float64) {
	pv, pok := q.lookup("per_page")
	gv, gok := q.lookup("page")
	perPage := math.Min(100, math.Max(1, orDefault(jsNumber(pv, pok), 30)))
	page := math.Max(1, orDefault(jsNumber(gv, gok), 1))
	return perPage, page
}

func jsSliceBounds(n int, start, end float64) (int, int) {
	clamp := func(x float64) int {
		x = math.Trunc(x)
		if x < 0 {
			x = math.Max(0, float64(n)+x)
		}
		if x > float64(n) {
			x = float64(n)
		}
		return int(x)
	}
	s, e := clamp(start), clamp(end)
	if e < s {
		e = s
	}
	return s, e
}

// paginate returns the [from, to) bounds of the page and the Link header.
func paginate(n int, ctx *Ctx) (int, int, []KV) {
	perPage, page := pageParams(ctx.Query)
	from, to := jsSliceBounds(n, (page-1)*perPage, page*perPage)
	last := math.Max(1, math.Ceil(float64(n)/perPage))
	link := func(p float64) string {
		q := ctx.Query.clone()
		q.set("per_page", jsNum(perPage))
		q.set("page", jsNum(p))
		return "<" + ctx.Origin + "/api/v3" + ctx.Path + "?" + q.String() + ">"
	}
	var rels []string
	if page < last {
		rels = append(rels, link(page+1)+"; rel=\"next\"", link(last)+"; rel=\"last\"")
	}
	if page > 1 {
		rels = append(rels, link(1)+"; rel=\"first\"", link(page-1)+"; rel=\"prev\"")
	}
	if len(rels) > 0 {
		return from, to, []KV{{"link", strings.Join(rels, ", ")}}
	}
	return from, to, nil
}

func ownRepo(owner, name string) *Repo {
	r := getRepo(owner, name)
	if r == nil || strings.ToLower(r.Owner) != strings.ToLower(owner) {
		throw(404, "Not Found")
	}
	return r
}

func issueOf(r *Repo, num string) *Issue {
	f := jsNumber(num, true)
	var i *Issue
	if !math.IsNaN(f) && f == math.Trunc(f) {
		i = r.ByNumber[int(f)]
	}
	if i == nil {
		throw(404, "Not Found")
	}
	return i
}

func pullOfRest(r *Repo, num string) *Pull {
	i := issueOf(r, num)
	if i.Pull == nil {
		throw(404, "Not Found")
	}
	return i.Pull
}

var wsRe = regexp.MustCompile(`\s+`)

func decodeContent(v any) []byte {
	s, ok := v.(string)
	if !ok {
		throw(422, "Invalid request.\n\n\"content\" wasn't supplied.")
	}
	out := lenientB64(wsRe.ReplaceAllString(s, ""))
	if out == nil {
		out = []byte{}
	}
	return out
}

func b64(b []byte) string { return stdB64(b) }

func contentView(r *Repo, path, oid, origin, ref string) O {
	blob := r.Objects[oid]
	name := path
	if k := strings.LastIndexByte(path, '/'); k >= 0 {
		name = path[k+1:]
	}
	api := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	size, content := 0, ""
	if blob != nil {
		size, content = len(blob.Data), b64(blob.Data)
	}
	return O{{"type", "file"}, {"encoding", "base64"}, {"size", size}, {"name", name}, {"path", path}, {"content", content}, {"sha", oid},
		{"url", api + "/contents/" + path + "?ref=" + ref}, {"git_url", api + "/git/blobs/" + oid},
		{"html_url", origin + "/" + r.Owner + "/" + r.Name + "/blob/" + ref + "/" + path}, {"download_url", nil}}
}

func entryView(r *Repo, dir string, e TreeEntry, origin string) O {
	path := e.Name
	if dir != "" {
		path = dir + "/" + e.Name
	}
	isDir := e.Mode == "40000"
	typ := "file"
	if isDir {
		typ = "dir"
	} else if e.Mode == "160000" {
		typ = "submodule"
	}
	size := 0
	if !isDir {
		if o := r.Objects[e.Oid]; o != nil {
			size = len(o.Data)
		}
	}
	return O{{"type", typ}, {"size", size}, {"name", e.Name}, {"path", path}, {"sha", e.Oid}, {"url", origin + "/api/v3/repos/" + r.Owner + "/" + r.Name + "/contents/" + path}}
}

func branchName(r *Repo, b any) string {
	if s, ok := b.(string); ok && s != "" {
		return strings.TrimPrefix(s, "refs/heads/")
	}
	return r.DefaultBranch
}

// ------------------------------------------------------------------ issue listing (shared with MCP)

func isoMs(s string) int64 {
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func listIssueItems(r *Repo, state, sortKey, dir, labels string, pulls bool) []*Issue {
	if state == "" {
		state = "open"
	}
	if sortKey == "" {
		sortKey = "created"
	}
	if dir == "" {
		dir = "desc"
	}
	var want []string
	for _, s := range strings.Split(labels, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			want = append(want, s)
		}
	}
	var items []*Issue
	for _, i := range r.Issues {
		if !(state == "all" || i.State == state) {
			continue
		}
		if !pulls && i.Pull != nil {
			continue
		}
		ok := true
		for _, w := range want {
			has := false
			for _, l := range i.Labels {
				if strings.ToLower(l.Name) == w {
					has = true
					break
				}
			}
			if !has {
				ok = false
				break
			}
		}
		if ok {
			items = append(items, i)
		}
	}
	keyOf := func(i *Issue) int64 {
		switch sortKey {
		case "updated":
			return isoMs(i.Updated)
		case "comments":
			return int64(len(i.Comments))
		}
		return int64(i.Seq)
	}
	sign := int64(-1)
	if dir == "asc" {
		sign = 1
	}
	sort.SliceStable(items, func(a, b int) bool {
		d := keyOf(items[a]) - keyOf(items[b])
		if d == 0 {
			d = int64(items[a].Seq - items[b].Seq)
		}
		return d*sign < 0
	})
	return items
}

func labelNameOf(raw any) (string, bool) {
	switch x := raw.(type) {
	case string:
		return x, true
	case map[string]any:
		s, ok := x["name"].(string)
		return s, ok
	}
	return "", false
}

func createIssueChecked(r *Repo, title any, titleOK bool, body any, labelNames any, labelsPresent bool) *Issue {
	t, ok := title.(string)
	if !titleOK || !ok || strings.TrimSpace(t) == "" {
		throw(422, "Validation Failed", KV{"errors", []O{{{"resource", "Issue"}, {"field", "title"}, {"code", "missing_field"}}}})
	}
	labels := []*Label{}
	if labelsPresent && labelNames != nil {
		arr, ok := labelNames.([]any)
		if !ok {
			throw(422, "Validation Failed: labels must be an array")
		}
		for _, raw := range arr {
			name, ok := labelNameOf(raw)
			var label *Label
			if ok {
				label = r.Labels[strings.ToLower(name)]
			}
			if label == nil {
				var shown any = raw
				if ok {
					shown = name
				}
				throw(422, "Validation Failed: label "+string(toJSON(shown))+" does not exist", KV{"errors", []O{{{"resource", "Label"}, {"field", "name"}, {"code", "invalid"}, {"value", shown}}}})
			}
			dup := false
			for _, x := range labels {
				if x == label {
					dup = true
				}
			}
			if !dup {
				labels = append(labels, label)
			}
		}
	}
	bs, _ := body.(string)
	return createIssue(r, t, bs, labels)
}

func createPullChecked(r *Repo, title, body, headRaw, baseRaw any) *Pull {
	hr, ok := headRaw.(string)
	if !ok || hr == "" {
		throw(422, "Validation Failed: head is required")
	}
	head := hr
	if k := strings.IndexByte(hr, ':'); k >= 0 {
		head = hr[k+1:]
	}
	base := r.DefaultBranch
	if b, ok := baseRaw.(string); ok && b != "" {
		base = b
	}
	if _, ok := r.Refs["refs/heads/"+head]; !ok {
		throw(422, "Validation Failed: head branch "+string(toJSON(head))+" does not exist", KV{"errors", []O{{{"resource", "PullRequest"}, {"field", "head"}, {"code", "invalid"}}}})
	}
	if _, ok := r.Refs["refs/heads/"+base]; !ok {
		throw(422, "Validation Failed: base branch "+string(toJSON(base))+" does not exist", KV{"errors", []O{{{"resource", "PullRequest"}, {"field", "base"}, {"code", "invalid"}}}})
	}
	for _, i := range r.Issues {
		if i.Pull != nil && i.State == "open" && i.Pull.Head == head && i.Pull.Base == base {
			msg := "A pull request already exists for " + r.Owner + ":" + head + "."
			throw(422, "Validation Failed: "+msg, KV{"errors", []O{{{"resource", "PullRequest"}, {"code", "custom"}, {"message", msg}}}})
		}
	}
	t := head
	if s, ok := title.(string); ok && s != "" {
		t = s
	}
	bs, _ := body.(string)
	return createPull(r, t, bs, head, base)
}

func createRepoChecked(name any, description any, private bool, autoInit bool) *Repo {
	n, ok := validRepoName(name)
	if !ok {
		throw(422, "Repository creation failed: name is invalid", KV{"errors", []O{{{"resource", "Repository"}, {"code", "custom"}, {"field", "name"}, {"message", "name is invalid"}}}})
	}
	if getRepo(VIEWER.Login, n) != nil {
		throw(422, "Repository creation failed: name already exists on this account", KV{"errors", []O{{{"resource", "Repository"}, {"code", "custom"}, {"field", "name"}, {"message", "name already exists on this account"}}}})
	}
	d, _ := description.(string)
	return createRepo(n, d, private, autoInit)
}

// ------------------------------------------------------------------ the router

type handlerFn func(ctx *Ctx, p []string) *Reply

type route struct {
	method string
	parts  []string
	h      handlerFn
}

var routes []route

func addRoute(method, pattern string, h handlerFn) {
	var parts []string
	for _, p := range strings.Split(pattern, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	routes = append(routes, route{method, parts, h})
}

func ok(body any, status int, headers []KV) *Reply { return &Reply{Status: status, Body: body, Headers: headers} }

func refView(r *Repo, ref, sha, origin string) O {
	typ := "commit"
	if o := r.Objects[sha]; o != nil {
		typ = o.Type
	}
	base := origin + "/api/v3/repos/" + r.Owner + "/" + r.Name
	return O{{"ref", ref}, {"node_id", globalID("REF", r.ID)}, {"url", base + "/git/" + ref}, {"object", O{{"sha", sha}, {"type", typ}, {"url", base + "/git/" + typ + "s/" + sha}}}}
}

func branchView(r *Repo, name, sha, origin string) O {
	return O{{"name", name}, {"commit", O{{"sha", sha}, {"url", origin + "/api/v3/repos/" + r.Owner + "/" + r.Name + "/commits/" + sha}}}, {"protected", false}}
}

func createLabelsList(r *Repo, names any) []*Label {
	arr, ok := names.([]any)
	if !ok {
		throw(422, "Validation Failed: labels must be an array")
	}
	out := []*Label{}
	for _, raw := range arr {
		var name string
		var ok bool
		if s, isStr := raw.(string); isStr {
			name, ok = s, true
		} else if m, isObj := raw.(map[string]any); isObj {
			name, ok = m["name"].(string)
		}
		var l *Label
		if ok {
			l = r.Labels[strings.ToLower(name)]
		}
		if l == nil {
			shown := "undefined"
			if ok {
				shown = string(toJSON(name))
			}
			throw(422, "Validation Failed: label "+shown+" does not exist")
		}
		out = append(out, l)
	}
	return out
}

func contentsGet(ctx *Ctx, o, n, path string) *Reply {
	r := ownRepo(o, n)
	ref, has := ctx.Query.lookup("ref")
	if !has {
		ref = r.DefaultBranch
	}
	commit, okc := resolveRev(r, ref)
	if !okc {
		if len(r.Refs) == 0 {
			throw(404, "This repository is empty.")
		}
		throw(404, "No commit found for the ref")
	}
	found := lookupPath(r, commit, path)
	if found == nil {
		throw(404, "Not Found")
	}
	if found.Kind == "file" {
		return ok(contentView(r, path, found.Oid, ctx.Origin, ref), 200, nil)
	}
	dir := strings.Trim(path, "/")
	list := make([]O, len(found.Entries))
	for k, e := range found.Entries {
		list[k] = entryView(r, dir, e, ctx.Origin)
	}
	return ok(list, 200, nil)
}

func getRef(ctx *Ctx, o, n, rest string) *Reply {
	r := ownRepo(o, n)
	ref := "refs/" + rest
	if sha, has := r.Refs[ref]; has {
		return ok(refView(r, ref, sha, ctx.Origin), 200, nil)
	}
	var list []O
	for _, e := range refEntries(r) {
		if strings.HasPrefix(e[0], ref+"/") {
			list = append(list, refView(r, e[0], e[1], ctx.Origin))
		}
	}
	if len(list) == 0 {
		throw(404, "Not Found")
	}
	return ok(list, 200, nil)
}

var readmeRe = regexp.MustCompile(`(?i)^readme(\.|$)`)
var refNameRe = regexp.MustCompile(`^refs/[^\s]+$`)

func init() {
	addRoute("GET", "/", func(ctx *Ctx, _ []string) *Reply {
		return ok(O{{"current_user_url", ctx.Origin + "/api/v3/user"}, {"repository_url", ctx.Origin + "/api/v3/repos/{owner}/{repo}"},
			{"user_repositories_url", ctx.Origin + "/api/v3/user/repos"}, {"issues_url", ctx.Origin + "/api/v3/issues"}, {"rate_limit_url", ctx.Origin + "/api/v3/rate_limit"}},
			200, []KV{{"x-oauth-scopes", "repo, read:org, workflow"}, {"x-accepted-oauth-scopes", ""}})
	})
	addRoute("GET", "/user", func(ctx *Ctx, _ []string) *Reply {
		return ok(userView(ctx.Origin), 200, []KV{{"x-oauth-scopes", "repo, read:org, workflow"}})
	})
	addRoute("GET", "/meta", func(ctx *Ctx, _ []string) *Reply {
		return ok(O{{"verifiable_password_authentication", false}, {"installed_version", "3.14.0-forge.1"}}, 200, nil)
	})
	addRoute("GET", "/rate_limit", func(ctx *Ctx, _ []string) *Reply {
		core := O{{"limit", 5000}, {"remaining", 5000}, {"reset", 0}, {"used", 0}}
		return ok(O{{"resources", O{{"core", core}}}, {"rate", O{{"limit", 5000}, {"remaining", 5000}, {"reset", 0}, {"used", 0}}}}, 200, nil)
	})
	addRoute("GET", "/users/:u", func(ctx *Ctx, p []string) *Reply {
		if strings.ToLower(p[0]) != strings.ToLower(VIEWER.Login) {
			throw(404, "Not Found")
		}
		return ok(userView(ctx.Origin), 200, nil)
	})
	addRoute("GET", "/user/repos", func(ctx *Ctx, _ []string) *Reply {
		all := listRepos(VIEWER.Login)
		sort.SliceStable(all, func(a, b int) bool { return strings.ToLower(all[a].Name) < strings.ToLower(all[b].Name) })
		from, to, h := paginate(len(all), ctx)
		out := make([]O, 0, to-from)
		for _, r := range all[from:to] {
			out = append(out, repoView(r, ctx.Origin))
		}
		return ok(out, 200, h)
	})
	addRoute("GET", "/users/:u/repos", func(ctx *Ctx, p []string) *Reply {
		all := listRepos(p[0])
		sort.SliceStable(all, func(a, b int) bool { return all[a].Name < all[b].Name })
		from, to, h := paginate(len(all), ctx)
		out := make([]O, 0, to-from)
		for _, r := range all[from:to] {
			out = append(out, repoView(r, ctx.Origin))
		}
		return ok(out, 200, h)
	})
	addRoute("POST", "/user/repos", func(ctx *Ctx, _ []string) *Reply {
		b := objBody(ctx)
		return ok(repoView(createRepoChecked(b["name"], b["description"], b["private"] == true || b["visibility"] == "private", b["auto_init"] == true), ctx.Origin), 201, nil)
	})
	addRoute("GET", "/repos/:o/:r", func(ctx *Ctx, p []string) *Reply { return ok(repoView(ownRepo(p[0], p[1]), ctx.Origin), 200, nil) })
	addRoute("PATCH", "/repos/:o/:r", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		b := objBody(ctx)
		if d, has := b["description"]; has {
			if s, isStr := d.(string); isStr {
				r.Description = s
			} else if d == nil {
				r.Description = ""
			}
		}
		if v, isBool := b["private"].(bool); isBool {
			r.Private = v
		}
		if s, isStr := b["default_branch"].(string); isStr && s != "" {
			r.DefaultBranch = s
		}
		touch(r)
		return ok(repoView(r, ctx.Origin), 200, nil)
	})
	addRoute("DELETE", "/repos/:o/:r", func(ctx *Ctx, p []string) *Reply {
		deleteRepo(ownRepo(p[0], p[1]))
		return &Reply{Status: 204}
	})

	// --- contents
	addRoute("GET", "/repos/:o/:r/contents", func(ctx *Ctx, p []string) *Reply { return contentsGet(ctx, p[0], p[1], "") })
	addRoute("GET", "/repos/:o/:r/contents/*", func(ctx *Ctx, p []string) *Reply { return contentsGet(ctx, p[0], p[1], p[2]) })
	addRoute("PUT", "/repos/:o/:r/contents/*", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		path := p[2]
		b := objBody(ctx)
		content := decodeContent(b["content"])
		branch := branchName(r, b["branch"])
		ref := "refs/heads/" + branch
		if _, has := r.Refs[ref]; !has && len(r.Refs) > 0 {
			from, hasFrom := r.Refs["refs/heads/"+r.DefaultBranch]
			if !hasFrom {
				throw(404, "Branch "+branch+" not found")
			}
			setRefTo(r, ref, from)
		}
		tip, hasTip := r.Refs[ref]
		var existing *pathResult
		if hasTip {
			existing = lookupPath(r, tip, path)
		}
		if existing != nil && existing.Kind == "dir" {
			throw(422, "path is a directory")
		}
		sha, hasSha := bstr(b, "sha")
		if existing != nil {
			if !hasSha {
				throw(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
			}
			if sha != existing.Oid {
				throw(409, path+" does not match "+sha)
			}
		} else if hasSha && hasTip {
			throw(409, path+" does not exist at "+sha)
		}
		message, hasMsg := bstr(b, "message")
		if !hasMsg {
			message = "Update " + path
		}
		commit, blobs := commitFiles(r, branch, []fileChange{{path, content}}, message)
		status := 200
		if existing == nil {
			status = 201
		}
		return ok(O{{"content", contentView(r, path, blobs[path], ctx.Origin, branch)}, {"commit", commitView(r, commit, ctx.Origin)}}, status, nil)
	})
	addRoute("DELETE", "/repos/:o/:r/contents/*", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		path := p[2]
		b := objBody(ctx)
		branch := branchName(r, b["branch"])
		tip, hasTip := r.Refs["refs/heads/"+branch]
		var existing *pathResult
		if hasTip {
			existing = lookupPath(r, tip, path)
		}
		if existing == nil || existing.Kind != "file" {
			throw(404, "Not Found")
		}
		sha, hasSha := bstr(b, "sha")
		if !hasSha {
			throw(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
		}
		if sha != existing.Oid {
			throw(409, path+" does not match "+sha)
		}
		message, hasMsg := bstr(b, "message")
		if !hasMsg {
			message = "Delete " + path
		}
		commit, _ := commitFiles(r, branch, []fileChange{{path, nil}}, message)
		return ok(O{{"content", nil}, {"commit", commitView(r, commit, ctx.Origin)}}, 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/readme", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		refq, hasRef := ctx.Query.lookup("ref")
		commit, okc := resolveRev(r, refq)
		if !okc {
			throw(404, "Not Found")
		}
		root := lookupPath(r, commit, "")
		var e *TreeEntry
		if root != nil && root.Kind == "dir" {
			for _, x := range root.Entries {
				if readmeRe.MatchString(x.Name) {
					x := x
					e = &x
					break
				}
			}
		}
		if e == nil {
			throw(404, "Not Found")
		}
		ref := r.DefaultBranch
		if hasRef {
			ref = refq
		}
		return ok(contentView(r, e.Name, e.Oid, ctx.Origin, ref), 200, nil)
	})

	// --- refs, branches, commits
	addRoute("POST", "/repos/:o/:r/git/refs", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		b := objBody(ctx)
		ref, hasRef := bstr(b, "ref")
		sha, hasSha := bstr(b, "sha")
		if !hasRef || !refNameRe.MatchString(ref) || len(strings.Split(ref, "/")) < 3 {
			throw(422, "Reference name is invalid")
		}
		if !hasSha || r.Objects[sha] == nil {
			throw(422, "Object does not exist")
		}
		if _, has := r.Refs[ref]; has {
			throw(422, "Reference already exists")
		}
		setRefTo(r, ref, sha)
		return ok(refView(r, ref, sha, ctx.Origin), 201, nil)
	})
	addRoute("GET", "/repos/:o/:r/git/refs/*", func(ctx *Ctx, p []string) *Reply { return getRef(ctx, p[0], p[1], p[2]) })
	addRoute("GET", "/repos/:o/:r/git/ref/*", func(ctx *Ctx, p []string) *Reply { return getRef(ctx, p[0], p[1], p[2]) })
	addRoute("GET", "/repos/:o/:r/git/refs", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		list := []O{}
		for _, e := range sortedRefEntriesJS(r) {
			list = append(list, refView(r, e[0], e[1], ctx.Origin))
		}
		return ok(list, 200, nil)
	})
	addRoute("PATCH", "/repos/:o/:r/git/refs/*", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		ref := "refs/" + p[2]
		cur, has := r.Refs[ref]
		if !has {
			throw(422, "Reference does not exist")
		}
		b := objBody(ctx)
		sha, hasSha := bstr(b, "sha")
		if !hasSha || r.Objects[sha] == nil {
			throw(422, "Object does not exist")
		}
		curC, ok1 := peelToCommit(r, cur)
		newC, ok2 := peelToCommit(r, sha)
		if b["force"] != true && (!ok1 || !ok2 || !isAncestor(r, curC, newC)) {
			throw(422, "Update is not a fast forward")
		}
		setRefTo(r, ref, sha)
		return ok(refView(r, ref, sha, ctx.Origin), 200, nil)
	})
	addRoute("DELETE", "/repos/:o/:r/git/refs/*", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		ref := "refs/" + p[2]
		if _, has := r.Refs[ref]; !has {
			throw(422, "Reference does not exist")
		}
		setRef(r, ref, nil)
		return &Reply{Status: 204}
	})
	addRoute("GET", "/repos/:o/:r/branches", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		var list []O
		for _, e := range sortedRefEntriesJS(r) {
			if strings.HasPrefix(e[0], "refs/heads/") {
				list = append(list, branchView(r, e[0][11:], e[1], ctx.Origin))
			}
		}
		from, to, h := paginate(len(list), ctx)
		out := []O{}
		if list != nil {
			out = list[from:to]
		}
		return ok(out, 200, h)
	})
	addRoute("GET", "/repos/:o/:r/branches/*", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		sha, has := r.Refs["refs/heads/"+p[2]]
		if !has {
			throw(404, "Branch not found")
		}
		v := branchView(r, p[2], sha, ctx.Origin)
		v = v.Set("commit", commitView(r, sha, ctx.Origin))
		return ok(v, 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/commits", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		if len(r.Refs) == 0 {
			throw(409, "Git Repository is empty.")
		}
		shaq, _ := ctx.Query.lookup("sha")
		tip, okt := resolveRev(r, shaq)
		if !okt {
			throw(404, "No commit found for SHA")
		}
		perPage, page := pageParams(ctx.Query)
		all := history(r, tip, int(math.Ceil(perPage*page+1)))
		from, to, h := paginate(len(all), ctx)
		slice := all[from:to]
		if path, has := ctx.Query.lookup("path"); has && path != "" {
			var filtered []string
			for _, sha := range slice {
				c := commitOf(r, sha)
				now := lookupPath(r, sha, path)
				var before *pathResult
				if c != nil && len(c.Parents) > 0 && c.Parents[0] != "" {
					before = lookupPath(r, c.Parents[0], path)
				}
				var a, bb *string
				if now != nil {
					a = &now.Oid
				}
				if before != nil {
					bb = &before.Oid
				}
				if (a == nil) != (bb == nil) || (a != nil && *a != *bb) {
					filtered = append(filtered, sha)
				}
			}
			slice = filtered
		}
		out := make([]O, 0, len(slice))
		for _, sha := range slice {
			out = append(out, commitView(r, sha, ctx.Origin))
		}
		return ok(out, 200, h)
	})
	addRoute("GET", "/repos/:o/:r/commits/:sha", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		c, okc := resolveRev(r, p[2])
		if !okc {
			throw(422, "No commit found for SHA: "+p[2])
		}
		return ok(commitView(r, c, ctx.Origin), 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/git/commits/:sha", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		if commitOf(r, p[2]) == nil {
			throw(404, "Not Found")
		}
		v := commitView(r, p[2], ctx.Origin)
		out := O{{"sha", p[2]}}
		cv, _ := v.Get("commit")
		for _, kv := range cv.(O) {
			out = out.Set(kv.K, kv.V)
		}
		parents, _ := v.Get("parents")
		out = out.Set("parents", parents)
		return ok(out, 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/git/trees/:sha", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		sha := p[2]
		commit, isCommit := resolveRev(r, sha)
		treeOid := sha
		if isCommit {
			c := commitOf(r, commit)
			if c == nil {
				throw(404, "Not Found")
			}
			treeOid = c.Tree
		}
		if o := r.Objects[treeOid]; o == nil || o.Type != "tree" {
			throw(404, "Not Found")
		}
		rec, hasRec := ctx.Query.lookup("recursive")
		tree := []O{}
		if hasRec && rec != "0" && rec != "false" {
			fm := flattenTree(r, treeOid, "", newFileMap())
			for _, path := range fm.keys {
				e := fm.m[path]
				size := 0
				if o := r.Objects[e.Oid]; o != nil {
					size = len(o.Data)
				}
				tree = append(tree, O{{"path", path}, {"mode", e.Mode}, {"type", "blob"}, {"sha", e.Oid}, {"size", size}})
			}
		} else {
			cm := ""
			if isCommit {
				cm = commit
			}
			if res := lookupPath(r, cm, ""); res != nil {
				for _, e := range res.Entries {
					typ := "blob"
					if e.Mode == "40000" {
						typ = "tree"
					}
					tree = append(tree, O{{"path", e.Name}, {"mode", e.Mode}, {"type", typ}, {"sha", e.Oid}})
				}
			}
		}
		return ok(O{{"sha", treeOid}, {"tree", tree}, {"truncated", false}}, 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/git/blobs/:sha", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		b := r.Objects[p[2]]
		if b == nil || b.Type != "blob" {
			throw(404, "Not Found")
		}
		return ok(O{{"sha", p[2]}, {"size", len(b.Data)}, {"encoding", "base64"}, {"content", b64(b.Data)}, {"url", ctx.Origin + "/api/v3/repos/" + r.Owner + "/" + r.Name + "/git/blobs/" + p[2]}}, 200, nil)
	})

	// --- labels
	addRoute("GET", "/repos/:o/:r/labels", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		out := []O{}
		for _, l := range r.labelOrder {
			out = append(out, labelView(l, r, ctx.Origin))
		}
		return ok(out, 200, nil)
	})
	addRoute("POST", "/repos/:o/:r/labels", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		b := objBody(ctx)
		name, hasName := bstr(b, "name")
		if !hasName || strings.TrimSpace(name) == "" {
			throw(422, "Validation Failed: name is required", KV{"errors", []O{{{"resource", "Label"}, {"field", "name"}, {"code", "missing_field"}}}})
		}
		if r.Labels[strings.ToLower(name)] != nil {
			throw(422, "Validation Failed: label already exists", KV{"errors", []O{{{"resource", "Label"}, {"field", "name"}, {"code", "already_exists"}}}})
		}
		color, hasColor := bstr(b, "color")
		if !hasColor {
			color = "ededed"
		}
		color = strings.TrimPrefix(color, "#")
		desc, _ := bstr(b, "description")
		return ok(labelView(addLabel(r, name, color, desc), r, ctx.Origin), 201, nil)
	})
	addRoute("GET", "/repos/:o/:r/labels/:name", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		l := r.Labels[strings.ToLower(p[2])]
		if l == nil {
			throw(404, "Not Found")
		}
		return ok(labelView(l, r, ctx.Origin), 200, nil)
	})

	// --- issues
	addRoute("GET", "/repos/:o/:r/issues", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		q := ctx.Query
		items := listIssueItems(r, q.get("state"), q.get("sort"), q.get("direction"), q.get("labels"), true)
		from, to, h := paginate(len(items), ctx)
		b := []byte{'['}
		for k, i := range items[from:to] {
			if k > 0 {
				b = append(b, ',')
			}
			b = append(b, issueJSON(i, ctx.Origin)...)
		}
		b = append(b, ']')
		return &Reply{Status: 200, Raw: b, Headers: h}
	})
	addRoute("POST", "/repos/:o/:r/issues", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		b := objBody(ctx)
		t, tok := b["title"]
		l, lok := b["labels"]
		i := createIssueChecked(r, t, tok, b["body"], l, lok)
		return &Reply{Status: 201, Raw: issueJSON(i, ctx.Origin)}
	})
	addRoute("GET", "/repos/:o/:r/issues/:n", func(ctx *Ctx, p []string) *Reply {
		return &Reply{Status: 200, Raw: issueJSON(issueOf(ownRepo(p[0], p[1]), p[2]), ctx.Origin)}
	})
	addRoute("PATCH", "/repos/:o/:r/issues/:n", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		i := issueOf(r, p[2])
		b := objBody(ctx)
		if s, isStr := b["title"].(string); isStr {
			i.Title = s
		}
		if v, has := b["body"]; has {
			if s, isStr := v.(string); isStr {
				i.Body = s
			} else if v == nil {
				i.Body = ""
			}
		}
		if v, has := b["labels"]; has {
			i.Labels = createLabelsList(r, v)
		}
		st, hasState := b["state"]
		if st == "open" || st == "closed" {
			setIssueState(i, st.(string))
		} else if hasState {
			throw(422, "Validation Failed: state must be open or closed")
		}
		i.Updated = nextISO()
		i.dirty()
		return ok(issueView(i, ctx.Origin), 200, nil)
	})
	addRoute("POST", "/repos/:o/:r/issues/:n/labels", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		i := issueOf(r, p[2])
		var list any
		if arr, isArr := ctx.Body.([]any); isArr {
			list = arr
		} else {
			list = objBody(ctx)["labels"]
		}
		for _, l := range createLabelsList(r, list) {
			dup := false
			for _, x := range i.Labels {
				if x == l {
					dup = true
				}
			}
			if !dup {
				i.Labels = append(i.Labels, l)
			}
		}
		i.dirty()
		out := make([]O, len(i.Labels))
		for k, l := range i.Labels {
			out[k] = labelView(l, r, ctx.Origin)
		}
		return ok(out, 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/issues/:n/comments", func(ctx *Ctx, p []string) *Reply {
		i := issueOf(ownRepo(p[0], p[1]), p[2])
		from, to, h := paginate(len(i.Comments), ctx)
		out := make([]O, 0, to-from)
		for _, c := range i.Comments[from:to] {
			out = append(out, commentView(c, ctx.Origin))
		}
		return ok(out, 200, h)
	})
	addRoute("POST", "/repos/:o/:r/issues/:n/comments", func(ctx *Ctx, p []string) *Reply {
		i := issueOf(ownRepo(p[0], p[1]), p[2])
		b := objBody(ctx)
		body, isStr := b["body"].(string)
		if !isStr || body == "" {
			throw(422, "Validation Failed: body is required")
		}
		return ok(commentView(addComment(i, body), ctx.Origin), 201, nil)
	})
	addRoute("GET", "/repos/:o/:r/issues/comments/:id", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		f := jsNumber(p[2], true)
		var c *Comment
		if !math.IsNaN(f) && f == math.Trunc(f) {
			c = commentsByID[int(f)]
		}
		if c == nil || c.Issue.Repo != r {
			throw(404, "Not Found")
		}
		return ok(commentView(c, ctx.Origin), 200, nil)
	})

	// --- pulls
	addRoute("GET", "/repos/:o/:r/pulls", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		state, hasState := ctx.Query.lookup("state")
		if !hasState {
			state = "open"
		}
		head, hasHead := ctx.Query.lookup("head")
		base, hasBase := ctx.Query.lookup("base")
		dir, hasDir := ctx.Query.lookup("direction")
		if !hasDir {
			dir = "desc"
		}
		headName := head
		if k := strings.IndexByte(head, ':'); k >= 0 {
			headName = head[k+1:]
		}
		var pulls []*Issue
		for _, i := range r.Issues {
			if i.Pull == nil || !(state == "all" || i.State == state) {
				continue
			}
			if hasHead && i.Pull.Head != headName {
				continue
			}
			if hasBase && i.Pull.Base != base {
				continue
			}
			pulls = append(pulls, i)
		}
		sort.SliceStable(pulls, func(a, b int) bool {
			if dir == "asc" {
				return pulls[a].Seq < pulls[b].Seq
			}
			return pulls[a].Seq > pulls[b].Seq
		})
		from, to, h := paginate(len(pulls), ctx)
		out := make([]O, 0, to-from)
		for _, i := range pulls[from:to] {
			out = append(out, pullView(i.Pull, ctx.Origin))
		}
		return ok(out, 200, h)
	})
	addRoute("POST", "/repos/:o/:r/pulls", func(ctx *Ctx, p []string) *Reply {
		r := ownRepo(p[0], p[1])
		b := objBody(ctx)
		return ok(pullView(createPullChecked(r, b["title"], b["body"], b["head"], b["base"]), ctx.Origin), 201, nil)
	})
	addRoute("GET", "/repos/:o/:r/pulls/:n", func(ctx *Ctx, p []string) *Reply {
		return ok(pullView(pullOfRest(ownRepo(p[0], p[1]), p[2]), ctx.Origin), 200, nil)
	})
	addRoute("PATCH", "/repos/:o/:r/pulls/:n", func(ctx *Ctx, p []string) *Reply {
		pr := pullOfRest(ownRepo(p[0], p[1]), p[2])
		b := objBody(ctx)
		if s, isStr := b["title"].(string); isStr {
			pr.Issue.Title = s
		}
		if s, isStr := b["body"].(string); isStr {
			pr.Issue.Body = s
		}
		if s, isStr := b["base"].(string); isStr {
			if _, has := pr.Issue.Repo.Refs["refs/heads/"+s]; has {
				pr.Base = s
			}
		}
		if st := b["state"]; (st == "open" || st == "closed") && !pr.Merged {
			setIssueState(pr.Issue, st.(string))
		}
		pr.Issue.dirty()
		return ok(pullView(pr, ctx.Origin), 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/pulls/:n/files", func(ctx *Ctx, p []string) *Reply {
		pr := pullOfRest(ownRepo(p[0], p[1]), p[2])
		r := pr.Issue.Repo
		files := pullFiles(pr)
		list := make([]O, len(files))
		for k, f := range files {
			list[k] = O{{"sha", f.Oid}, {"filename", f.Path}, {"status", f.Status}, {"additions", f.Additions}, {"deletions", f.Deletions}, {"changes", f.Additions + f.Deletions},
				{"blob_url", ctx.Origin + "/" + r.Owner + "/" + r.Name + "/blob/" + pr.HeadSha + "/" + f.Path},
				{"contents_url", ctx.Origin + "/api/v3/repos/" + r.Owner + "/" + r.Name + "/contents/" + f.Path + "?ref=" + pr.HeadSha}}
		}
		from, to, h := paginate(len(list), ctx)
		return ok(list[from:to], 200, h)
	})
	addRoute("GET", "/repos/:o/:r/pulls/:n/commits", func(ctx *Ctx, p []string) *Reply {
		pr := pullOfRest(ownRepo(p[0], p[1]), p[2])
		r := pr.Issue.Repo
		head, base := pullShas(pr)
		var out []string
		for _, sha := range history(r, head, 250) {
			if isAncestor(r, sha, base) {
				continue
			}
			out = append(out, sha)
		}
		list := make([]O, 0, len(out))
		for k := len(out) - 1; k >= 0; k-- {
			list = append(list, commitView(r, out[k], ctx.Origin))
		}
		return ok(list, 200, nil)
	})
	addRoute("GET", "/repos/:o/:r/pulls/:n/merge", func(ctx *Ctx, p []string) *Reply {
		pr := pullOfRest(ownRepo(p[0], p[1]), p[2])
		if !pr.Merged {
			throw(404, "Not Found")
		}
		return &Reply{Status: 204}
	})
	addRoute("PUT", "/repos/:o/:r/pulls/:n/merge", func(ctx *Ctx, p []string) *Reply {
		pr := pullOfRest(ownRepo(p[0], p[1]), p[2])
		b := objBody(ctx)
		if sha, has := bstr(b, "sha"); has {
			head, _ := pullShas(pr)
			if sha != head {
				throw(409, "Head branch was modified. Review and try the merge again.")
			}
		}
		method, hasMethod := bstr(b, "merge_method")
		if !hasMethod {
			method = "merge"
		}
		var title, msg *string
		if s, has := bstr(b, "commit_title"); has {
			title = &s
		}
		if s, has := bstr(b, "commit_message"); has {
			msg = &s
		}
		sha, status, message := mergePull(pr, method, title, msg)
		if status != 0 {
			throw(status, message)
		}
		return ok(O{{"sha", sha}, {"merged", true}, {"message", "Pull Request successfully merged"}}, 200, nil)
	})
}

// ------------------------------------------------------------------ dispatch

var routeIndex map[string][]route

func candidates(method string, count int) []route {
	if routeIndex == nil {
		routeIndex = map[string][]route{}
		for _, rt := range routes {
			wild := isWild(rt)
			k := rt.method + ":" + strconv.Itoa(len(rt.parts))
			if wild {
				k = rt.method + ":*"
			}
			routeIndex[k] = append(routeIndex[k], rt)
		}
	}
	key := method + "#" + strconv.Itoa(count)
	combined, has := routeIndex[key]
	if !has {
		combined = append(append([]route(nil), routeIndex[method+":"+strconv.Itoa(count)]...), routeIndex[method+":*"]...)
		routeIndex[key] = combined
	}
	return combined
}

func dispatch(ctx *Ctx) (reply *Reply, he *HttpError) {
	defer func() {
		if p := recover(); p != nil {
			if e, isH := p.(*HttpError); isH {
				reply, he = nil, e
				return
			}
			panic(p)
		}
	}()
	var parts []string
	for _, p := range strings.Split(ctx.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	for _, rt := range candidates(ctx.Method, len(parts)) {
		var params []string
		matched := true
		i := 0
		wild := isWild(rt)
		for ; i < len(rt.parts); i++ {
			want := rt.parts[i]
			if want == "*" {
				if i >= len(parts) {
					matched = false
					break
				}
				dec := make([]string, 0, len(parts)-i)
				for _, s := range parts[i:] {
					dec = append(dec, mustDecode(s))
				}
				params = append(params, strings.Join(dec, "/"))
				i = len(parts)
				break
			}
			if i >= len(parts) {
				matched = false
				break
			}
			got := parts[i]
			if strings.HasPrefix(want, ":") {
				params = append(params, mustDecode(got))
			} else if want != got {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if !wild && len(parts) != len(rt.parts) {
			continue
		}
		if wild && i < len(parts) {
			continue
		}
		return rt.h(ctx, params), nil
	}
	return nil, herr(404, "Not Found")
}

func isWild(rt route) bool { return len(rt.parts) > 0 && rt.parts[len(rt.parts)-1] == "*" }
