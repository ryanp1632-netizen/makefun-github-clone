// Model Context Protocol at POST /mcp: JSON-RPC 2.0 over streamable HTTP (the 2026-07-28 revision's
// server/discover, plus the initialize handshake). Tools act on the same store as REST.

package main

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var mcpSupported = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func serverInfo() O {
	return O{{"name", "makefun-forge"}, {"title", "Forge MCP"}, {"version", "1.0.0"}}
}

const mcpInstructions = "A GitHub-compatible forge. Repositories belong to the authenticated user; use owner + repo to address one."

type toolError string

func toolFail(msg string) { panic(toolError(msg)) }

type toolDef struct {
	name, description string
	props             O
	required          []string
	run               func(a map[string]any, origin string) any
}

func sprop(desc string) O { return O{{"type", "string"}, {"description", desc}} }
func nprop(desc string) O { return O{{"type", "number"}, {"description", desc}} }
func bprop(desc string) O { return O{{"type", "boolean"}, {"description", desc}} }

var ownerProp = O{{"type", "string"}, {"description", "Repository owner (user login)"}}
var repoProp = O{{"type", "string"}, {"description", "Repository name"}}

func repoArg(a map[string]any) *Repo {
	owner, okO := a["owner"].(string)
	name, okN := a["repo"].(string)
	if fn, isStr := a["full_name"].(string); isStr && strings.Contains(fn, "/") {
		parts := strings.SplitN(fn, "/", 3)
		owner, name, okO, okN = parts[0], parts[1], true, true
	}
	if !okO || !okN {
		toolFail("owner and repo are required")
	}
	r := getRepo(owner, name)
	if r == nil || strings.ToLower(r.Owner) != strings.ToLower(owner) {
		toolFail("repository " + owner + "/" + name + " not found")
	}
	return r
}

var digitsRe = regexp.MustCompile(`^\d+$`)

func numArg(v any, what string) int {
	if s, isStr := v.(string); isStr && digitsRe.MatchString(s) {
		f, _ := strconv.ParseFloat(s, 64)
		v = f
	}
	f, isNum := v.(float64)
	if !isNum || f != math.Trunc(f) || f < 1 || math.IsInf(f, 0) {
		toolFail(what + " must be a positive integer")
	}
	return int(f)
}

func mcpPullOf(r *Repo, n int) *Pull {
	i := r.ByNumber[n]
	if i == nil || i.Pull == nil {
		toolFail("pull request #" + strconv.Itoa(n) + " not found in " + r.Owner + "/" + r.Name)
	}
	return i.Pull
}

func optStr(a map[string]any, k string) *string {
	if s, isStr := a[k].(string); isStr {
		return &s
	}
	return nil
}

var tools = []toolDef{
	{"create_repo", "Create a new repository owned by the authenticated user.",
		O{{"name", sprop("Repository name")}, {"description", sprop("Repository description")}, {"private", bprop("Whether the repository is private")}, {"autoInit", bprop("Create an initial commit with a README")}},
		[]string{"name"},
		func(a map[string]any, o string) any {
			return repoView(createRepoChecked(a["name"], a["description"], a["private"] == true, a["autoInit"] == true), o)
		}},
	{"get_repo", "Get a repository by owner and name.", O{{"owner", ownerProp}, {"repo", repoProp}}, []string{"owner", "repo"},
		func(a map[string]any, o string) any { return repoView(repoArg(a), o) }},
	{"list_repos", "List the repositories of the authenticated user.", O{{"perPage", nprop("Results per page (max 100)")}}, []string{},
		func(a map[string]any, o string) any {
			list := []O{}
			for _, r := range listRepos(VIEWER.Login) {
				list = append(list, repoView(r, o))
			}
			return O{{"repositories", list}}
		}},
	{"create_issue", "Create a new issue in a repository.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"title", sprop("Issue title")}, {"body", sprop("Issue body (Markdown)")}, {"labels", O{{"type", "array"}, {"items", O{{"type", "string"}}}, {"description", "Label names (each must exist)"}}}},
		[]string{"owner", "repo", "title"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			t, tok := a["title"]
			l, lok := a["labels"]
			return issueView(createIssueChecked(r, t, tok, a["body"], l, lok), o)
		}},
	{"get_issue", "Get one issue by number.", O{{"owner", ownerProp}, {"repo", repoProp}, {"issue_number", nprop("Issue number")}}, []string{"owner", "repo", "issue_number"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			i := r.ByNumber[numArg(a["issue_number"], "issue_number")]
			if i == nil {
				toolFail("issue not found")
			}
			return issueView(i, o)
		}},
	{"list_issues", "List issues in a repository, filtered by state.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"state", O{{"type", "string"}, {"enum", []string{"open", "closed", "all"}}, {"description", "Issue state filter (default open)"}}},
			{"labels", O{{"type", "array"}, {"items", O{{"type", "string"}}}, {"description", "Only issues carrying all these labels"}}},
			{"direction", O{{"type", "string"}, {"enum", []string{"asc", "desc"}}, {"description", "Sort direction by creation"}}}},
		[]string{"owner", "repo"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			state := "open"
			if s, isStr := a["state"].(string); isStr {
				state = strings.ToLower(s)
			}
			dir := "desc"
			if s, isStr := a["direction"].(string); isStr {
				dir = s
			}
			labels := ""
			if arr, isArr := a["labels"].([]any); isArr {
				labels = jsString(arr, true)
			}
			items := listIssueItems(r, state, "", dir, labels, false)
			list := make([]O, len(items))
			for k, i := range items {
				list[k] = issueView(i, o)
			}
			return O{{"issues", list}, {"totalCount", len(items)}}
		}},
	{"update_issue", "Update an issue: title, body or state (open/closed).",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"issue_number", nprop("Issue number")}, {"title", sprop("New title")}, {"body", sprop("New body")},
			{"state", O{{"type", "string"}, {"enum", []string{"open", "closed"}}, {"description", "New state"}}}},
		[]string{"owner", "repo", "issue_number"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			i := r.ByNumber[numArg(a["issue_number"], "issue_number")]
			if i == nil {
				toolFail("issue not found")
			}
			if s, isStr := a["title"].(string); isStr {
				i.Title = s
			}
			if s, isStr := a["body"].(string); isStr {
				i.Body = s
			}
			if st := a["state"]; st == "open" || st == "closed" {
				setIssueState(i, st.(string))
			}
			i.dirty()
			return issueView(i, o)
		}},
	{"add_issue_comment", "Add a comment to an issue or pull request.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"issue_number", nprop("Issue or pull request number")}, {"body", sprop("Comment text")}},
		[]string{"owner", "repo", "issue_number", "body"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			i := r.ByNumber[numArg(a["issue_number"], "issue_number")]
			if i == nil {
				toolFail("issue not found")
			}
			body, isStr := a["body"].(string)
			if !isStr || body == "" {
				toolFail("body is required")
			}
			c := addComment(i, body)
			return O{{"id", c.ID}, {"body", c.Body}, {"created_at", c.Created}, {"issue_number", i.Number}}
		}},
	{"create_branch", "Create a branch from another branch (default: the default branch).",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"branch", sprop("New branch name")}, {"from_branch", sprop("Source branch")}},
		[]string{"owner", "repo", "branch"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			branch, isStr := a["branch"].(string)
			if !isStr || branch == "" {
				toolFail("branch is required")
			}
			from := r.DefaultBranch
			if s, isStr := a["from_branch"].(string); isStr && s != "" {
				from = s
			}
			sha, has := r.Refs["refs/heads/"+from]
			if !has {
				toolFail("branch " + from + " not found")
			}
			if _, exists := r.Refs["refs/heads/"+branch]; exists {
				toolFail("branch " + branch + " already exists")
			}
			setRefTo(r, "refs/heads/"+branch, sha)
			return O{{"ref", "refs/heads/" + branch}, {"object", O{{"sha", sha}, {"type", "commit"}}}}
		}},
	{"list_branches", "List the branches of a repository.", O{{"owner", ownerProp}, {"repo", repoProp}}, []string{"owner", "repo"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			list := []O{}
			for _, e := range sortedRefEntriesJS(r) {
				if strings.HasPrefix(e[0], "refs/heads/") {
					list = append(list, O{{"name", e[0][11:]}, {"commit", O{{"sha", e[1]}}}})
				}
			}
			return O{{"branches", list}}
		}},
	{"create_or_update_file", "Create or update one file on a branch with a commit (pass sha to update an existing file).",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"path", sprop("File path")}, {"content", sprop("File content (plain text)")}, {"message", sprop("Commit message")},
			{"branch", sprop("Branch (default: the default branch)")}, {"sha", sprop("Current blob sha when updating")}},
		[]string{"owner", "repo", "path", "content", "message"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			path, okP := a["path"].(string)
			content, okC := a["content"].(string)
			if !okP || path == "" || !okC {
				toolFail("path and content are required")
			}
			branch := r.DefaultBranch
			if s, isStr := a["branch"].(string); isStr && s != "" {
				branch = s
			}
			if _, has := r.Refs["refs/heads/"+branch]; !has && len(r.Refs) > 0 {
				from, hasFrom := r.Refs["refs/heads/"+r.DefaultBranch]
				if !hasFrom {
					toolFail("branch " + branch + " not found")
				}
				setRefTo(r, "refs/heads/"+branch, from)
			}
			tip, hasTip := r.Refs["refs/heads/"+branch]
			var existing *pathResult
			if hasTip {
				existing = lookupPath(r, tip, path)
			}
			if sha, isStr := a["sha"].(string); existing != nil && existing.Kind == "file" && isStr && sha != existing.Oid {
				toolFail("sha does not match " + path)
			}
			message := "Update " + path
			if s, isStr := a["message"].(string); isStr {
				message = s
			}
			commit, blobs := commitFiles(r, branch, []fileChange{{path, []byte(content)}}, message)
			return O{{"content", O{{"path", path}, {"sha", blobs[path]}}}, {"commit", O{{"sha", commit}}}}
		}},
	{"get_file_contents", "Get a file (decoded text) or a directory listing at a path and ref.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"path", sprop("Path (empty for the root)")}, {"ref", sprop("Branch, tag or commit sha")}},
		[]string{"owner", "repo"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			ref, _ := a["ref"].(string)
			commit, okc := resolveRev(r, ref)
			if !okc {
				toolFail("ref not found or repository empty")
			}
			path := ""
			if s, isStr := a["path"].(string); isStr {
				path = strings.TrimLeft(s, "/")
			}
			found := lookupPath(r, commit, path)
			if found == nil {
				toolFail(path + " not found")
			}
			if found.Kind == "file" {
				content := ""
				if ob := r.Objects[found.Oid]; ob != nil {
					content = toValidUTF8(ob.Data)
				}
				return O{{"type", "file"}, {"path", path}, {"sha", found.Oid}, {"content", content}}
			}
			entries := make([]O, len(found.Entries))
			for k, e := range found.Entries {
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				}
				entries[k] = O{{"name", e.Name}, {"type", typ}, {"sha", e.Oid}}
			}
			return O{{"type", "dir"}, {"path", path}, {"entries", entries}}
		}},
	{"list_commits", "List commits reachable from a branch or sha, newest first.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"sha", sprop("Branch or commit sha (default: the default branch)")}, {"perPage", nprop("Max commits (default 30)")}},
		[]string{"owner", "repo"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			sha, _ := a["sha"].(string)
			tip, okt := resolveRev(r, sha)
			if !okt {
				return O{{"commits", []O{}}}
			}
			limit := 30.0
			if f, isNum := a["perPage"].(float64); isNum {
				limit = math.Min(100, math.Max(1, f))
			}
			list := []O{}
			for _, s := range history(r, tip, int(math.Ceil(limit))) {
				msg := ""
				if c := commitOf(r, s); c != nil {
					msg = c.Message
				}
				list = append(list, O{{"sha", s}, {"message", strings.TrimSuffix(msg, "\n")}})
			}
			return O{{"commits", list}}
		}},
	{"create_pull_request", "Open a pull request from a head branch into a base branch.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"title", sprop("Pull request title")}, {"body", sprop("Pull request description")}, {"head", sprop("Branch with the changes")}, {"base", sprop("Branch to merge into")}},
		[]string{"owner", "repo", "title", "head", "base"},
		func(a map[string]any, o string) any {
			return pullView(createPullChecked(repoArg(a), a["title"], a["body"], a["head"], a["base"]), o)
		}},
	{"get_pull_request", "Get one pull request by number.", O{{"owner", ownerProp}, {"repo", repoProp}, {"pull_number", nprop("Pull request number")}}, []string{"owner", "repo", "pull_number"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			return pullView(mcpPullOf(r, numArg(a["pull_number"], "pull_number")), o)
		}},
	{"list_pull_requests", "List pull requests in a repository, filtered by state.",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"state", O{{"type", "string"}, {"enum", []string{"open", "closed", "all"}}, {"description", "State filter (default open)"}}}},
		[]string{"owner", "repo"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			state := "open"
			if s, isStr := a["state"].(string); isStr {
				state = s
			}
			list := []O{}
			for _, i := range r.Issues {
				if i.Pull != nil && (state == "all" || i.State == state) {
					list = append(list, pullView(i.Pull, o))
				}
			}
			return O{{"pull_requests", list}}
		}},
	{"get_pull_request_files", "List the files a pull request changes.", O{{"owner", ownerProp}, {"repo", repoProp}, {"pull_number", nprop("Pull request number")}}, []string{"owner", "repo", "pull_number"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			files := pullFiles(mcpPullOf(r, numArg(a["pull_number"], "pull_number")))
			list := make([]O, len(files))
			for k, f := range files {
				list[k] = O{{"filename", f.Path}, {"status", f.Status}, {"additions", f.Additions}, {"deletions", f.Deletions}, {"changes", f.Additions + f.Deletions}}
			}
			return O{{"files", list}}
		}},
	{"merge_pull_request", "Merge a pull request (merge_method: merge or squash).",
		O{{"owner", ownerProp}, {"repo", repoProp}, {"pull_number", nprop("Pull request number")},
			{"merge_method", O{{"type", "string"}, {"enum", []string{"merge", "squash", "rebase"}}, {"description", "Merge method (default squash)"}}},
			{"commit_title", sprop("Merge commit title")}, {"commit_message", sprop("Merge commit message")}},
		[]string{"owner", "repo", "pull_number"},
		func(a map[string]any, o string) any {
			r := repoArg(a)
			p := mcpPullOf(r, numArg(a["pull_number"], "pull_number"))
			method := "squash"
			if s, isStr := a["merge_method"].(string); isStr {
				method = s
			}
			sha, status, msg := mergePull(p, method, optStr(a, "commit_title"), optStr(a, "commit_message"))
			if status != 0 {
				toolFail(msg)
			}
			return O{{"sha", sha}, {"merged", true}, {"message", "Pull Request successfully merged"}, {"number", p.Issue.Number}}
		}},
}

var toolList []O
var toolsByName = map[string]*toolDef{}

func init() {
	for k := range tools {
		t := &tools[k]
		words := strings.Split(t.name, "_")
		for i, w := range words {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
		req := t.required
		if req == nil {
			req = []string{}
		}
		toolList = append(toolList, O{{"name", t.name}, {"title", strings.Join(words, " ")}, {"description", t.description},
			{"inputSchema", O{{"type", "object"}, {"properties", t.props}, {"required", req}, {"additionalProperties", false}}}})
		toolsByName[t.name] = t
	}
}

func rpcError(id any, code int, message string) O {
	return O{{"jsonrpc", "2.0"}, {"id", id}, {"error", O{{"code", code}, {"message", message}}}}
}

func toolResult(value any, isError bool) O {
	if isError {
		return O{{"content", []O{{{"type", "text"}, {"text", value.(string)}}}}, {"isError", true}}
	}
	structured := value
	if _, isObj := value.(O); !isObj {
		structured = O{{"result", value}}
	}
	return O{{"content", []O{{{"type", "text"}, {"text", string(toJSON(value))}}}}, {"structuredContent", structured}, {"isError", false}}
}

func runTool(t *toolDef, args map[string]any, origin string) (res O) {
	defer func() {
		if p := recover(); p != nil {
			switch e := p.(type) {
			case toolError:
				res = toolResult(string(e), true)
			case *HttpError:
				res = toolResult(e.Message, true)
			default:
				res = toolResult("internal error: "+panicMessage(p), true)
			}
		}
	}()
	return toolResult(t.run(args, origin), false)
}

func handleOne(msg any, origin string) O {
	m, isObj := msg.(map[string]any)
	if !isObj {
		return rpcError(nil, -32600, "Invalid Request")
	}
	var id any
	switch v := m["id"].(type) {
	case string, float64:
		id = v
	}
	_, hasID := m["id"]
	isNotification := !hasID
	method, okM := m["method"].(string)
	if m["jsonrpc"] != "2.0" || !okM {
		if isNotification {
			return nil
		}
		return rpcError(id, -32600, "Invalid Request")
	}
	params, isP := m["params"].(map[string]any)
	if !isP {
		params = map[string]any{}
	}
	if isNotification {
		return nil
	}
	switch method {
	case "server/discover":
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", O{{"supportedVersions", mcpSupported}, {"capabilities", O{{"tools", O{{"listChanged", false}}}}},
			{"serverInfo", serverInfo()}, {"instructions", mcpInstructions}, {"_meta", O{{"io.modelcontextprotocol/serverInfo", serverInfo()}}}}}}
	case "initialize":
		asked, _ := params["protocolVersion"].(string)
		version := "2025-11-25"
		for _, v := range mcpSupported {
			if v == asked && asked != "2026-07-28" {
				version = asked
			}
		}
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", O{{"protocolVersion", version}, {"capabilities", O{{"tools", O{{"listChanged", false}}}}}, {"serverInfo", serverInfo()}, {"instructions", mcpInstructions}}}}
	case "ping":
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", O{}}}
	case "tools/list":
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", O{{"tools", toolList}}}}
	case "resources/list":
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", O{{"resources", []O{}}}}}
	case "prompts/list":
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", O{{"prompts", []O{}}}}}
	case "tools/call":
		name, hasName := params["name"]
		ns, isStr := name.(string)
		var t *toolDef
		if isStr {
			t = toolsByName[ns]
		}
		if t == nil {
			return rpcError(id, -32602, "Unknown tool: "+jsString(name, hasName))
		}
		args, isA := params["arguments"].(map[string]any)
		if !isA {
			args = map[string]any{}
		}
		var missing []string
		for _, k := range t.required {
			v, has := args[k]
			if !has || v == nil || v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", toolResult("missing required argument(s): "+strings.Join(missing, ", "), true)}}
		}
		return O{{"jsonrpc", "2.0"}, {"id", id}, {"result", runTool(t, args, origin)}}
	}
	return rpcError(id, -32601, "Method not found: "+method)
}

// handleMcp answers one POST /mcp body: an HTTP status and the JSON reply (nil = 202 Accepted).
func handleMcp(raw []byte, origin string) (int, any) {
	msg, err := parseJSON([]byte(toValidUTF8(raw)))
	if err != nil {
		return 400, rpcError(nil, -32700, "Parse error")
	}
	if arr, isArr := msg.([]any); isArr {
		if len(arr) == 0 {
			return 400, rpcError(nil, -32600, "Invalid Request")
		}
		var out []any
		for _, x := range arr {
			if r := handleOne(x, origin); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return 202, nil
		}
		return 200, out
	}
	out := handleOne(msg, origin)
	if out == nil {
		return 202, nil
	}
	return 200, out
}
