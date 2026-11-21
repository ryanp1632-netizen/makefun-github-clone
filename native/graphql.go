// GitHub GraphQL v4 at /api/graphql: a small spec-shaped engine (lexer, parser, executor and
// introspection) over the schema in SDL below, with resolvers on the shared store.

package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ------------------------------------------------------------------ lexer

type tok struct {
	k string // name punct string int float eof
	v string
}

var numRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`)

func isNameStart(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' }
func isNameChar(c byte) bool  { return isNameStart(c) || c >= '0' && c <= '9' }

func gqlLex(src string) ([]tok, error) {
	var out []tok
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if strings.HasPrefix(src[i:], "\xef\xbb\xbf") {
			i += 3
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && strings.HasPrefix(src[i:], "...") {
			out = append(out, tok{"punct", "..."})
			i += 3
			continue
		}
		if strings.IndexByte("!$&()[]{}:=@|", c) >= 0 {
			out = append(out, tok{"punct", string(c)})
			i++
			continue
		}
		if isNameStart(c) {
			j := i + 1
			for j < n && isNameChar(src[j]) {
				j++
			}
			out = append(out, tok{"name", src[i:j]})
			i = j
			continue
		}
		if c == '-' || c >= '0' && c <= '9' {
			m := numRe.FindStringSubmatch(src[i:])
			if m == nil {
				return nil, fmt.Errorf("Unexpected character %s", string(toJSON(string(c))))
			}
			k := "int"
			if m[2] != "" || m[3] != "" {
				k = "float"
			}
			out = append(out, tok{k, m[0]})
			i += len(m[0])
			continue
		}
		if c == '"' {
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, errors.New("Unterminated block string")
				}
				raw := src[i+3 : i+3+end]
				lines := regexp.MustCompile(`\r?\n`).Split(raw, -1)
				indent := math.MaxInt
				for _, line := range lines[1:] {
					w := 0
					for w < len(line) && (line[w] == ' ' || line[w] == '\t') {
						w++
					}
					if w < len(line) && w < indent {
						indent = w
					}
				}
				trimmed := make([]string, len(lines))
				for k, l := range lines {
					if k == 0 || indent == math.MaxInt {
						trimmed[k] = l
					} else if len(l) > indent {
						trimmed[k] = l[indent:]
					} else {
						trimmed[k] = ""
					}
				}
				for len(trimmed) > 0 && strings.TrimSpace(trimmed[0]) == "" {
					trimmed = trimmed[1:]
				}
				for len(trimmed) > 0 && strings.TrimSpace(trimmed[len(trimmed)-1]) == "" {
					trimmed = trimmed[:len(trimmed)-1]
				}
				out = append(out, tok{"string", strings.Join(trimmed, "\n")})
				i += 3 + end + 3
				continue
			}
			j := i + 1
			var units []uint16
			var sb strings.Builder
			flush := func() {
				if len(units) > 0 {
					sb.WriteString(string(utf16.Decode(units)))
					units = units[:0]
				}
			}
			for j < n && src[j] != '"' {
				if src[j] == '\\' {
					if j+1 >= n {
						j++
						break
					}
					e := src[j+1]
					if e == 'u' {
						h := ""
						if j+6 <= n {
							h = src[j+2 : j+6]
						}
						v, err := strconv.ParseUint(h, 16, 16)
						if err != nil {
							v = 0
						}
						units = append(units, uint16(v))
						j += 6
						continue
					}
					flush()
					switch e {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					default:
						sb.WriteByte(e)
					}
					j += 2
					continue
				}
				if src[j] == '\n' {
					return nil, errors.New("Unterminated string")
				}
				flush()
				sb.WriteByte(src[j])
				j++
			}
			flush()
			if j >= n {
				return nil, errors.New("Unterminated string")
			}
			out = append(out, tok{"string", sb.String()})
			i = j + 1
			continue
		}
		r := []rune(src[i:])[0]
		return nil, fmt.Errorf("Unexpected character %s", string(toJSON(string(r))))
	}
	return append(out, tok{"eof", ""}), nil
}

// ------------------------------------------------------------------ AST

type gValue struct {
	kind   string // var int float string boolean null enum list object
	name   string
	val    any
	items  []*gValue
	fields []gArg
}

type gArg struct {
	name string
	v    *gValue
}

type selection struct {
	kind   byte // 'f' field, 'i' inline, 's' spread
	alias  string
	name   string
	args   []gArg
	sel    []*selection
	hasSel bool
	on     string
	hasOn  bool
}

type varDef struct {
	name string
	def  *gValue
}

type operation struct {
	op      string
	name    string
	hasName bool
	vars    []varDef
	sel     []*selection
}

type fragment struct {
	on  string
	sel []*selection
}

type document struct {
	ops   []*operation
	frags map[string]*fragment
}

type gqlParser struct {
	t []tok
	p int
}

type parseErr string

func perr(msg string) { panic(parseErr(msg)) }

func tokDesc(t tok) string {
	if t.k == "eof" {
		return "<EOF>"
	}
	return string(toJSON(t.v))
}

func (ps *gqlParser) peek(v string) bool {
	t := ps.t[ps.p]
	return (t.k == "punct" || t.k == "name") && t.v == v
}

func (ps *gqlParser) next() tok {
	t := ps.t[ps.p]
	if ps.p < len(ps.t)-1 {
		ps.p++
	}
	return t
}

func (ps *gqlParser) expect(v string) {
	t := ps.next()
	if t.v != v || (t.k != "punct" && t.k != "name") {
		perr("Expected " + string(toJSON(v)) + ", found " + tokDesc(t))
	}
}

func (ps *gqlParser) name() string {
	t := ps.next()
	if t.k != "name" {
		perr("Expected a name, found " + tokDesc(t))
	}
	return t.v
}

func (ps *gqlParser) atEOF() bool { return ps.t[ps.p].k == "eof" }

func (ps *gqlParser) doc() *document {
	d := &document{frags: map[string]*fragment{}}
	for !ps.atEOF() {
		if ps.peek("{") {
			d.ops = append(d.ops, &operation{op: "query", sel: ps.selSet()})
		} else if ps.peek("fragment") {
			ps.next()
			name := ps.name()
			ps.expect("on")
			on := ps.name()
			ps.directives()
			d.frags[name] = &fragment{on: on, sel: ps.selSet()}
		} else {
			opTok := ps.name()
			if opTok != "query" && opTok != "mutation" && opTok != "subscription" {
				perr("Unexpected " + string(toJSON(opTok)))
			}
			op := &operation{op: opTok}
			if ps.t[ps.p].k == "name" {
				op.name, op.hasName = ps.name(), true
			}
			if ps.peek("(") {
				ps.next()
				for !ps.peek(")") {
					if ps.atEOF() {
						perr("Expected \"$\", found <EOF>")
					}
					ps.expect("$")
					v := ps.name()
					ps.expect(":")
					ps.typeRef()
					var def *gValue
					if ps.peek("=") {
						ps.next()
						def = ps.value(true)
					}
					ps.directives()
					op.vars = append(op.vars, varDef{v, def})
				}
				ps.next()
			}
			ps.directives()
			op.sel = ps.selSet()
			d.ops = append(d.ops, op)
		}
	}
	return d
}

func (ps *gqlParser) typeRef() string {
	var s string
	if ps.peek("[") {
		ps.next()
		s = "[" + ps.typeRef() + "]"
		ps.expect("]")
	} else {
		s = ps.name()
	}
	if ps.peek("!") {
		ps.next()
		s += "!"
	}
	return s
}

func (ps *gqlParser) directives() {
	for ps.peek("@") {
		ps.next()
		ps.name()
		if ps.peek("(") {
			ps.args()
		}
	}
}

func (ps *gqlParser) selSet() []*selection {
	ps.expect("{")
	out := []*selection{}
	for !ps.peek("}") {
		if ps.atEOF() {
			perr("Unexpected <EOF>")
		}
		if ps.peek("...") {
			ps.next()
			if ps.peek("on") {
				ps.next()
				on := ps.name()
				ps.directives()
				out = append(out, &selection{kind: 'i', on: on, hasOn: true, sel: ps.selSet(), hasSel: true})
			} else if ps.peek("{") || ps.peek("@") {
				ps.directives()
				out = append(out, &selection{kind: 'i', sel: ps.selSet(), hasSel: true})
			} else {
				out = append(out, &selection{kind: 's', name: ps.name()})
				ps.directives()
			}
			continue
		}
		name := ps.name()
		alias := ""
		if ps.peek(":") {
			ps.next()
			alias = name
			name = ps.name()
		}
		var args []gArg
		if ps.peek("(") {
			args = ps.args()
		}
		ps.directives()
		s := &selection{kind: 'f', alias: alias, name: name, args: args}
		if ps.peek("{") {
			s.sel, s.hasSel = ps.selSet(), true
		}
		out = append(out, s)
	}
	ps.next()
	return out
}

func (ps *gqlParser) args() []gArg {
	ps.expect("(")
	var out []gArg
	for !ps.peek(")") {
		if ps.atEOF() {
			perr("Expected a name, found <EOF>")
		}
		n := ps.name()
		ps.expect(":")
		out = append(out, gArg{n, ps.value(false)})
	}
	ps.next()
	return out
}

func (ps *gqlParser) value(isConst bool) *gValue {
	t := ps.next()
	switch t.k {
	case "punct":
		if t.v == "$" && !isConst {
			return &gValue{kind: "var", name: ps.name()}
		}
		if t.v == "[" {
			v := &gValue{kind: "list", items: []*gValue{}}
			for !ps.peek("]") {
				if ps.atEOF() {
					perr("Unexpected <EOF>")
				}
				v.items = append(v.items, ps.value(isConst))
			}
			ps.next()
			return v
		}
		if t.v == "{" {
			v := &gValue{kind: "object"}
			for !ps.peek("}") {
				if ps.atEOF() {
					perr("Expected a name, found <EOF>")
				}
				n := ps.name()
				ps.expect(":")
				v.fields = append(v.fields, gArg{n, ps.value(isConst)})
			}
			ps.next()
			return v
		}
		perr("Unexpected " + string(toJSON(t.v)))
	case "int", "float":
		f, _ := strconv.ParseFloat(t.v, 64)
		return &gValue{kind: t.k, val: f}
	case "string":
		return &gValue{kind: "string", val: t.v}
	case "name":
		switch t.v {
		case "true", "false":
			return &gValue{kind: "boolean", val: t.v == "true"}
		case "null":
			return &gValue{kind: "null"}
		}
		return &gValue{kind: "enum", val: t.v}
	}
	perr("Unexpected <EOF>")
	return nil
}

func parseGql(src string) (d *document, err error) {
	defer func() {
		if p := recover(); p != nil {
			if pe, isPE := p.(parseErr); isPE {
				err = errors.New(string(pe))
				return
			}
			panic(p)
		}
	}()
	toks, lerr := gqlLex(src)
	if lerr != nil {
		return nil, lerr
	}
	ps := &gqlParser{t: toks}
	return ps.doc(), nil
}

// ------------------------------------------------------------------ schema (SDL)

const sdl = `
interface Node { id: ID! }
interface Actor { login: String! url: URI! avatarUrl: URI! }
interface Comment { id: ID! body: String! author: Actor createdAt: DateTime! }
interface Closable { closed: Boolean! closedAt: DateTime }
interface RepositoryOwner { id: ID! login: String! url: URI! }
interface Labelable { labels(first: Int, after: String): LabelConnection }
union IssueOrPullRequest = Issue | PullRequest
union SearchResultItem = Issue | PullRequest | Repository | User

type Query {
  viewer: User!
  repository(owner: String!, name: String!, followRenames: Boolean): Repository
  repositoryOwner(login: String!): RepositoryOwner
  user(login: String!): User
  node(id: ID!): Node
  nodes(ids: [ID!]!): [Node]!
  rateLimit(dryRun: Boolean): RateLimit
  search(query: String!, type: SearchType!, first: Int, after: String): SearchResultItemConnection!
}

type Mutation {
  createIssue(input: CreateIssueInput!): CreateIssuePayload
  updateIssue(input: UpdateIssueInput!): UpdateIssuePayload
  closeIssue(input: CloseIssueInput!): CloseIssuePayload
  reopenIssue(input: ReopenIssueInput!): ReopenIssuePayload
  addComment(input: AddCommentInput!): AddCommentPayload
  createPullRequest(input: CreatePullRequestInput!): CreatePullRequestPayload
  mergePullRequest(input: MergePullRequestInput!): MergePullRequestPayload
  closePullRequest(input: ClosePullRequestInput!): ClosePullRequestPayload
  createRepository(input: CreateRepositoryInput!): CreateRepositoryPayload
}

type RateLimit { limit: Int! cost: Int! remaining: Int! resetAt: DateTime! used: Int! }

type User implements Node & Actor & RepositoryOwner {
  id: ID! databaseId: Int login: String! name: String email: String! url: URI! avatarUrl(size: Int): URI!
  repositories(first: Int, after: String): RepositoryConnection!
}

type Repository implements Node {
  id: ID! databaseId: Int name: String! nameWithOwner: String! owner: RepositoryOwner! description: String
  url: URI! sshUrl: String! isPrivate: Boolean! visibility: RepositoryVisibility! isFork: Boolean! isArchived: Boolean!
  isEmpty: Boolean! createdAt: DateTime! updatedAt: DateTime! pushedAt: DateTime
  defaultBranchRef: Ref viewerPermission: RepositoryPermission hasIssuesEnabled: Boolean! hasWikiEnabled: Boolean!
  hasProjectsEnabled: Boolean! viewerCanAdminister: Boolean! mergeCommitAllowed: Boolean! squashMergeAllowed: Boolean! rebaseMergeAllowed: Boolean!
  issue(number: Int!): Issue
  issues(first: Int, last: Int, after: String, before: String, states: [IssueState!], orderBy: IssueOrder, labels: [String!], filterBy: IssueFilters): IssueConnection!
  issueOrPullRequest(number: Int!): IssueOrPullRequest
  pullRequest(number: Int!): PullRequest
  pullRequests(first: Int, last: Int, after: String, before: String, states: [PullRequestState!], orderBy: IssueOrder, headRefName: String, baseRefName: String, labels: [String!]): PullRequestConnection!
  ref(qualifiedName: String!): Ref
  refs(first: Int, after: String, refPrefix: String!): RefConnection
  label(name: String!): Label
  labels(first: Int, after: String): LabelConnection
}

type Issue implements Node & Comment & Closable & Labelable {
  id: ID! databaseId: Int number: Int! title: String! body: String! bodyText: String! state: IssueState! stateReason: IssueClosedStateReason
  url: URI! createdAt: DateTime! updatedAt: DateTime! closed: Boolean! closedAt: DateTime author: Actor repository: Repository!
  labels(first: Int, after: String): LabelConnection comments(first: Int, after: String, last: Int): IssueCommentConnection!
  viewerCanUpdate: Boolean! locked: Boolean!
}

type PullRequest implements Node & Comment & Closable & Labelable {
  id: ID! databaseId: Int number: Int! title: String! body: String! state: PullRequestState! url: URI!
  createdAt: DateTime! updatedAt: DateTime! closed: Boolean! closedAt: DateTime merged: Boolean! mergedAt: DateTime
  mergeable: MergeableState! headRefName: String! baseRefName: String! headRefOid: GitObjectID! baseRefOid: GitObjectID!
  isDraft: Boolean! author: Actor repository: Repository! headRepository: Repository baseRepository: Repository
  mergeCommit: Commit additions: Int! deletions: Int! changedFiles: Int!
  files(first: Int, after: String): PullRequestChangedFileConnection
  labels(first: Int, after: String): LabelConnection comments(first: Int, after: String, last: Int): IssueCommentConnection!
  locked: Boolean!
}

type IssueComment implements Node & Comment { id: ID! databaseId: Int body: String! author: Actor createdAt: DateTime! url: URI! issue: Issue! }
type Label implements Node { id: ID! name: String! color: String! description: String }
type Ref implements Node { id: ID! name: String! prefix: String! target: Commit repository: Repository! }
type Commit implements Node { id: ID! oid: GitObjectID! message: String! messageHeadline: String! committedDate: DateTime! url: URI! }
type PullRequestChangedFile { path: String! additions: Int! deletions: Int! changeType: PatchStatus! }

type PageInfo { hasNextPage: Boolean! hasPreviousPage: Boolean! startCursor: String endCursor: String }
type IssueConnection { nodes: [Issue] edges: [IssueEdge] pageInfo: PageInfo! totalCount: Int! }
type IssueEdge { cursor: String! node: Issue }
type PullRequestConnection { nodes: [PullRequest] edges: [PullRequestEdge] pageInfo: PageInfo! totalCount: Int! }
type PullRequestEdge { cursor: String! node: PullRequest }
type RepositoryConnection { nodes: [Repository] edges: [RepositoryEdge] pageInfo: PageInfo! totalCount: Int! }
type RepositoryEdge { cursor: String! node: Repository }
type LabelConnection { nodes: [Label] edges: [LabelEdge] pageInfo: PageInfo! totalCount: Int! }
type LabelEdge { cursor: String! node: Label }
type IssueCommentConnection { nodes: [IssueComment] edges: [IssueCommentEdge] pageInfo: PageInfo! totalCount: Int! }
type IssueCommentEdge { cursor: String! node: IssueComment }
type RefConnection { nodes: [Ref] edges: [RefEdge] pageInfo: PageInfo! totalCount: Int! }
type RefEdge { cursor: String! node: Ref }
type PullRequestChangedFileConnection { nodes: [PullRequestChangedFile] edges: [PullRequestChangedFileEdge] pageInfo: PageInfo! totalCount: Int! }
type PullRequestChangedFileEdge { cursor: String! node: PullRequestChangedFile }
type SearchResultItemConnection { nodes: [SearchResultItem] edges: [SearchResultItemEdge] pageInfo: PageInfo! issueCount: Int! repositoryCount: Int! }
type SearchResultItemEdge { cursor: String! node: SearchResultItem }

type CreateIssuePayload { clientMutationId: String issue: Issue }
type UpdateIssuePayload { clientMutationId: String issue: Issue }
type CloseIssuePayload { clientMutationId: String issue: Issue }
type ReopenIssuePayload { clientMutationId: String issue: Issue }
type AddCommentPayload { clientMutationId: String commentEdge: IssueCommentEdge subject: Node timelineEdge: IssueCommentEdge }
type CreatePullRequestPayload { clientMutationId: String pullRequest: PullRequest }
type MergePullRequestPayload { clientMutationId: String pullRequest: PullRequest }
type ClosePullRequestPayload { clientMutationId: String pullRequest: PullRequest }
type CreateRepositoryPayload { clientMutationId: String repository: Repository }

input CreateIssueInput { repositoryId: ID! title: String! body: String labelIds: [ID!] assigneeIds: [ID!] clientMutationId: String }
input UpdateIssueInput { id: ID! title: String body: String state: IssueState labelIds: [ID!] clientMutationId: String }
input CloseIssueInput { issueId: ID! stateReason: IssueClosedStateReason clientMutationId: String }
input ReopenIssueInput { issueId: ID! clientMutationId: String }
input AddCommentInput { subjectId: ID! body: String! clientMutationId: String }
input CreatePullRequestInput { repositoryId: ID! baseRefName: String! headRefName: String! title: String! body: String draft: Boolean clientMutationId: String }
input MergePullRequestInput { pullRequestId: ID! mergeMethod: PullRequestMergeMethod commitHeadline: String commitBody: String expectedHeadOid: GitObjectID clientMutationId: String }
input ClosePullRequestInput { pullRequestId: ID! clientMutationId: String }
input CreateRepositoryInput { name: String! description: String visibility: RepositoryVisibility! clientMutationId: String }
input IssueOrder { field: IssueOrderField! direction: OrderDirection! }
input IssueFilters { states: [IssueState!] labels: [String!] }

enum IssueState { OPEN CLOSED }
enum IssueClosedStateReason { COMPLETED NOT_PLANNED DUPLICATE REOPENED }
enum PullRequestState { OPEN CLOSED MERGED }
enum MergeableState { MERGEABLE CONFLICTING UNKNOWN }
enum PullRequestMergeMethod { MERGE SQUASH REBASE }
enum RepositoryPermission { ADMIN MAINTAIN WRITE TRIAGE READ }
enum RepositoryVisibility { PRIVATE PUBLIC INTERNAL }
enum SearchType { ISSUE ISSUE_ADVANCED REPOSITORY USER DISCUSSION }
enum IssueOrderField { CREATED_AT UPDATED_AT COMMENTS }
enum OrderDirection { ASC DESC }
enum PatchStatus { ADDED DELETED MODIFIED RENAMED COPIED CHANGED }

scalar ID
scalar String
scalar Int
scalar Float
scalar Boolean
scalar URI
scalar DateTime
scalar GitObjectID
`

type fieldDef struct {
	name string
	typ  string
	args []gArgDef
}

type gArgDef struct{ name, typ string }

type typeDef struct {
	name       string
	kind       string
	fieldOrder []*fieldDef
	fields     map[string]*fieldDef
	interfaces []string
	values     []string
	members    []string
}

var typeOrder []*typeDef
var types = map[string]*typeDef{}

func init() {
	toks, err := gqlLex(sdl)
	if err != nil {
		panic(err)
	}
	p := &gqlParser{t: toks}
	fieldList := func() ([]*fieldDef, map[string]*fieldDef) {
		var order []*fieldDef
		m := map[string]*fieldDef{}
		p.expect("{")
		for !p.peek("}") {
			name := p.name()
			var args []gArgDef
			if p.peek("(") {
				p.next()
				for !p.peek(")") {
					an := p.name()
					p.expect(":")
					args = append(args, gArgDef{an, p.typeRef()})
				}
				p.next()
			}
			p.expect(":")
			f := &fieldDef{name: name, typ: p.typeRef(), args: args}
			if _, dup := m[name]; !dup {
				order = append(order, f)
			}
			m[name] = f
		}
		p.next()
		return order, m
	}
	for !p.atEOF() {
		kw := p.name()
		name := p.name()
		d := &typeDef{name: name, kind: "OBJECT", fields: map[string]*fieldDef{}}
		switch kw {
		case "type", "interface", "input":
			switch kw {
			case "type":
				d.kind = "OBJECT"
			case "interface":
				d.kind = "INTERFACE"
			default:
				d.kind = "INPUT_OBJECT"
			}
			if p.peek("implements") {
				p.next()
				d.interfaces = append(d.interfaces, p.name())
				for p.peek("&") {
					p.next()
					d.interfaces = append(d.interfaces, p.name())
				}
			}
			d.fieldOrder, d.fields = fieldList()
		case "enum":
			d.kind = "ENUM"
			p.expect("{")
			for !p.peek("}") {
				d.values = append(d.values, p.name())
			}
			p.next()
		case "union":
			d.kind = "UNION"
			p.expect("=")
			d.members = append(d.members, p.name())
			for p.peek("|") {
				p.next()
				d.members = append(d.members, p.name())
			}
		case "scalar":
			d.kind = "SCALAR"
		default:
			panic("bad SDL keyword " + kw)
		}
		if _, dup := types[name]; !dup {
			typeOrder = append(typeOrder, d)
		} else {
			for k, t := range typeOrder {
				if t.name == name {
					typeOrder[k] = d
				}
			}
		}
		types[name] = d
	}
}

// ------------------------------------------------------------------ introspection objects

type gType struct {
	kind   string
	name   *string
	ofType *gType
	def    *typeDef
}

func typeRefObj(ref string) *gType {
	if strings.HasSuffix(ref, "!") {
		return &gType{kind: "NON_NULL", ofType: typeRefObj(ref[:len(ref)-1])}
	}
	if strings.HasPrefix(ref, "[") {
		return &gType{kind: "LIST", ofType: typeRefObj(ref[1 : len(ref)-1])}
	}
	d := types[ref]
	kind := "SCALAR"
	if d != nil {
		kind = d.kind
	}
	n := ref
	return &gType{kind: kind, name: &n, def: d}
}

var introspectionTypes = map[string]map[string]string{
	"__Schema":     {"queryType": "__Type!", "mutationType": "__Type", "subscriptionType": "__Type", "types": "[__Type!]!", "directives": "[__Directive!]!", "description": "String"},
	"__Type":       {"kind": "__TypeKind!", "name": "String", "description": "String", "fields": "[__Field!]", "interfaces": "[__Type!]", "possibleTypes": "[__Type!]", "enumValues": "[__EnumValue!]", "inputFields": "[__InputValue!]", "ofType": "__Type", "specifiedByURL": "String"},
	"__Field":      {"name": "String!", "description": "String", "args": "[__InputValue!]!", "type": "__Type!", "isDeprecated": "Boolean!", "deprecationReason": "String"},
	"__InputValue": {"name": "String!", "description": "String", "type": "__Type!", "defaultValue": "String", "isDeprecated": "Boolean!", "deprecationReason": "String"},
	"__EnumValue":  {"name": "String!", "description": "String", "isDeprecated": "Boolean!", "deprecationReason": "String"},
	"__Directive":  {"name": "String!", "description": "String", "locations": "[String!]!", "args": "[__InputValue!]!", "isRepeatable": "Boolean!"},
}

func inputValueObj(name, typ string) map[string]any {
	return map[string]any{"__t": "__InputValue", "name": name, "description": nil, "type": typeRefObj(typ), "defaultValue": nil, "isDeprecated": false, "deprecationReason": nil}
}

func introspect(typeName, field string, parent any) any {
	if typeName == "__Schema" {
		switch field {
		case "queryType":
			return typeRefObj("Query")
		case "mutationType":
			return typeRefObj("Mutation")
		case "types":
			out := make([]any, len(typeOrder))
			for k, t := range typeOrder {
				out[k] = typeRefObj(t.name)
			}
			return out
		case "directives":
			return []any{}
		}
		return nil
	}
	if typeName == "__Type" {
		t, isT := parent.(*gType)
		if !isT {
			return nil
		}
		d := t.def
		switch field {
		case "kind":
			return t.kind
		case "name":
			if t.name == nil {
				return nil
			}
			return *t.name
		case "ofType":
			if t.ofType == nil {
				return nil
			}
			return t.ofType
		case "fields":
			if d == nil || (d.kind != "OBJECT" && d.kind != "INTERFACE") {
				return nil
			}
			out := make([]any, len(d.fieldOrder))
			for k, f := range d.fieldOrder {
				args := make([]any, len(f.args))
				for j, a := range f.args {
					args[j] = inputValueObj(a.name, a.typ)
				}
				out[k] = map[string]any{"__t": "__Field", "name": f.name, "description": nil, "args": args, "type": typeRefObj(f.typ), "isDeprecated": false, "deprecationReason": nil}
			}
			return out
		case "inputFields":
			if d == nil || d.kind != "INPUT_OBJECT" {
				return nil
			}
			out := make([]any, len(d.fieldOrder))
			for k, f := range d.fieldOrder {
				out[k] = inputValueObj(f.name, f.typ)
			}
			return out
		case "interfaces":
			if d == nil || d.kind != "OBJECT" {
				if d != nil && d.kind == "INTERFACE" {
					return []any{}
				}
				return nil
			}
			out := make([]any, len(d.interfaces))
			for k, n := range d.interfaces {
				out[k] = typeRefObj(n)
			}
			return out
		case "possibleTypes":
			if d == nil {
				return nil
			}
			if d.kind == "UNION" {
				out := make([]any, len(d.members))
				for k, n := range d.members {
					out[k] = typeRefObj(n)
				}
				return out
			}
			if d.kind == "INTERFACE" {
				out := []any{}
				for _, x := range typeOrder {
					for _, i := range x.interfaces {
						if i == d.name {
							out = append(out, typeRefObj(x.name))
							break
						}
					}
				}
				return out
			}
			return nil
		case "enumValues":
			if d == nil || d.kind != "ENUM" {
				return nil
			}
			out := make([]any, len(d.values))
			for k, v := range d.values {
				out[k] = map[string]any{"__t": "__EnumValue", "name": v, "description": nil, "isDeprecated": false, "deprecationReason": nil}
			}
			return out
		}
		return nil
	}
	if m, isM := parent.(map[string]any); isM {
		return m[field]
	}
	return nil
}

// ------------------------------------------------------------------ resolvers

type gqlError struct{ msg, typ string }

func gfail(msg, typ string) { panic(&gqlError{msg, typ}) }

type viewerT struct{}

var viewerNode = &viewerT{}

type refNode struct {
	repo    *Repo
	refName string
	sha     string
}

type commitNode struct {
	repo *Repo
	sha  string
}

func cursorOf(i float64) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:v2:" + jsNum(i)))
}

var cursorRe = regexp.MustCompile(`^cursor:v2:(\d+)$`)

func decodeCursor(c string) (float64, bool) {
	m := cursorRe.FindStringSubmatch(toValidUTF8(lenientB64(c)))
	if m == nil {
		return 0, false
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return f, true
}

func connection(items []any, args map[string]any) map[string]any {
	start := 0.0
	if after, isStr := args["after"].(string); isStr && after != "" {
		idx, okc := decodeCursor(after)
		if !okc {
			gfail("`"+after+"` does not appear to be a valid cursor.", "INVALID_CURSOR_ARGUMENTS")
		}
		start = idx + 1
	}
	end := float64(len(items))
	if before, isStr := args["before"].(string); isStr && before != "" {
		if idx, okc := decodeCursor(before); okc {
			end = math.Min(end, idx)
		}
	}
	from, to := start, end
	first, hasFirst := args["first"].(float64)
	last, hasLast := args["last"].(float64)
	if hasFirst {
		if first < 0 || first > 100 {
			gfail("Requesting more than 100 records on the connection is not allowed.", "EXCESSIVE_PAGINATION")
		}
		to = math.Min(to, from+first)
	}
	if hasLast {
		from = math.Max(from, to-last)
	}
	if !hasFirst && !hasLast {
		to = math.Min(to, from+100)
	}
	s, e := jsSliceBounds(len(items), from, to)
	slice := items[s:e]
	edges := make([]any, len(slice))
	for k, node := range slice {
		edges[k] = map[string]any{"cursor": cursorOf(from + float64(k)), "node": node}
	}
	var startC, endC any
	if len(slice) > 0 {
		startC = cursorOf(from)
		endC = cursorOf(to - 1)
	}
	return map[string]any{
		"nodes": append([]any{}, slice...), "edges": edges, "totalCount": len(items),
		"pageInfo": map[string]any{"hasNextPage": to < end, "hasPreviousPage": from > 0, "startCursor": startC, "endCursor": endC},
	}
}

var gqlOrigin = "http://localhost"

func repoURL(r *Repo) string { return gqlOrigin + "/" + r.Owner + "/" + r.Name }

func gOwnRepo(owner, name any) *Repo {
	o, ok1 := owner.(string)
	n, ok2 := name.(string)
	if !ok1 || !ok2 {
		return nil
	}
	r := getRepo(o, n)
	if r != nil && strings.ToLower(r.Owner) == strings.ToLower(o) {
		return r
	}
	return nil
}

func nodeByID(id any) any {
	s, isStr := id.(string)
	if !isStr {
		return nil
	}
	kind, n, okd := decodeGlobalID(s)
	if !okd {
		return nil
	}
	switch kind {
	case "R":
		if r := reposByID[n]; r != nil {
			return r
		}
	case "I":
		if i := issuesByID[n]; i != nil {
			return i
		}
	case "PR":
		if p := pullsByID[n]; p != nil {
			return p
		}
	case "IC":
		if c := commentsByID[n]; c != nil {
			return c
		}
	case "U":
		if n == VIEWER.ID {
			return viewerNode
		}
	}
	return nil
}

func typeOfValue(v any) string {
	switch x := v.(type) {
	case *viewerT:
		return "User"
	case map[string]any:
		if t, isStr := x["__t"].(string); isStr {
			return t
		}
	case *gType:
		return "__Type"
	case *Repo:
		return "Repository"
	case *Issue:
		return "Issue"
	case *Pull:
		return "PullRequest"
	case *Comment:
		return "IssueComment"
	case *Label:
		return "Label"
	case *refNode:
		return "Ref"
	case *commitNode:
		return "Commit"
	}
	return "Unknown"
}

func issueStatesArg(a map[string]any) map[string]bool {
	states, has := a["states"]
	if !has || states == nil {
		if fb, isM := a["filterBy"].(map[string]any); isM {
			states = fb["states"]
		}
	}
	arr, isArr := states.([]any)
	if !isArr || len(arr) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, s := range arr {
		set[jsString(s, true)] = true
	}
	return set
}

func sortIssues(items []*Issue, order any, defaultDir string) []*Issue {
	o, _ := order.(map[string]any)
	field, isStr := o["field"].(string)
	if !isStr {
		field = "CREATED_AT"
	}
	dir, isStr := o["direction"].(string)
	if !isStr {
		dir = defaultDir
	}
	key := func(i *Issue) int64 {
		switch field {
		case "UPDATED_AT":
			return isoMs(i.Updated)
		case "COMMENTS":
			return int64(len(i.Comments))
		}
		return int64(i.Seq)
	}
	out := append([]*Issue(nil), items...)
	sign := int64(1)
	if dir == "DESC" {
		sign = -1
	}
	sort.SliceStable(out, func(a, b int) bool {
		d := key(out[a]) - key(out[b])
		if d == 0 {
			d = int64(out[a].Seq - out[b].Seq)
		}
		return d*sign < 0
	})
	return out
}

func pullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.Issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func issueByNumber(r *Repo, n any) *Issue {
	f, isNum := n.(float64)
	if !isNum || f != math.Trunc(f) {
		return nil
	}
	return r.ByNumber[int(f)]
}

func inputOf(a map[string]any) map[string]any {
	m, _ := a["input"].(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func cmid(input map[string]any) any { return input["clientMutationId"] }

func notResolvable(input map[string]any, k string) {
	v, has := input[k]
	gfail("Could not resolve to a node with the global id of '"+jsString(v, has)+"'", "NOT_FOUND")
}

func issuesToAny(items []*Issue) []any {
	out := make([]any, len(items))
	for k, i := range items {
		out[k] = i
	}
	return out
}

func labelsToAny(items []*Label) []any {
	out := make([]any, len(items))
	for k, l := range items {
		out[k] = l
	}
	return out
}

func commentsToAny(items []*Comment) []any {
	out := make([]any, len(items))
	for k, c := range items {
		out[k] = c
	}
	return out
}

func strOrNil(v any) *string {
	if s, isStr := v.(string); isStr {
		return &s
	}
	return nil
}

var refPrefixRe = regexp.MustCompile(`^refs/(heads|tags)/`)
var repoQRe = regexp.MustCompile(`repo:([^\s/]+)/(\S+)`)
var isOnlyRe = regexp.MustCompile(`is:(issue|pr|pull-request)`)
var stateOnlyRe = regexp.MustCompile(`is:(open|closed)`)

// resolve runs the resolver for typeName.field; found=false means "use the default (parent[field])".
func resolve(typeName, field string, parent any, a map[string]any) (any, bool) {
	switch typeName {
	case "Query":
		switch field {
		case "viewer":
			return viewerNode, true
		case "repository":
			if r := gOwnRepo(a["owner"], a["name"]); r != nil {
				return r, true
			}
			return nil, true
		case "repositoryOwner", "user":
			if l, isStr := a["login"].(string); isStr && strings.ToLower(l) == strings.ToLower(VIEWER.Login) {
				return viewerNode, true
			}
			return nil, true
		case "node":
			return nodeByID(a["id"]), true
		case "nodes":
			arr, isArr := a["ids"].([]any)
			if !isArr {
				return []any{}, true
			}
			out := make([]any, len(arr))
			for k, id := range arr {
				out[k] = nodeByID(id)
			}
			return out, true
		case "rateLimit":
			return map[string]any{"__t": "RateLimit", "limit": 5000, "cost": 1, "remaining": 5000, "resetAt": "2026-01-01T00:00:00Z", "used": 0}, true
		case "search":
			qv, has := a["query"]
			q := ""
			if has && qv != nil {
				q = jsString(qv, true)
			}
			var terms []string
			for _, t := range regexp.MustCompile(`\s+`).Split(q, -1) {
				if t != "" && !strings.Contains(t, ":") {
					terms = append(terms, strings.ToLower(t))
				}
			}
			repoMatch := repoQRe.FindStringSubmatch(q)
			isOnly := ""
			if m := isOnlyRe.FindStringSubmatch(q); m != nil {
				isOnly = m[1]
			}
			stateOnly := ""
			if m := stateOnlyRe.FindStringSubmatch(q); m != nil {
				stateOnly = m[1]
			}
			typ := a["type"]
			items := []any{}
			switch typ {
			case "REPOSITORY":
				for _, r := range listRepos(VIEWER.Login) {
					okAll := true
					for _, t := range terms {
						if !strings.Contains(strings.ToLower(r.Name), t) {
							okAll = false
							break
						}
					}
					if okAll {
						items = append(items, r)
					}
				}
			case "USER":
				items = append(items, viewerNode)
			default:
				var rs []*Repo
				if repoMatch != nil {
					if r := gOwnRepo(repoMatch[1], repoMatch[2]); r != nil {
						rs = []*Repo{r}
					}
				} else {
					rs = listRepos(VIEWER.Login)
				}
				for _, r := range rs {
					for _, i := range r.Issues {
						if isOnly == "issue" && i.Pull != nil {
							continue
						}
						if (isOnly == "pr" || isOnly == "pull-request") && i.Pull == nil {
							continue
						}
						if stateOnly != "" && i.State != stateOnly {
							continue
						}
						text := strings.ToLower(i.Title + " " + i.Body)
						okAll := true
						for _, t := range terms {
							if !strings.Contains(text, t) {
								okAll = false
								break
							}
						}
						if !okAll {
							continue
						}
						if i.Pull != nil {
							items = append(items, i.Pull)
						} else {
							items = append(items, i)
						}
					}
				}
			}
			c := connection(items, a)
			issueCount, repoCount := len(items), 0
			if typ == "REPOSITORY" || typ == "USER" {
				issueCount = 0
			}
			if typ == "REPOSITORY" {
				repoCount = len(items)
			}
			c["issueCount"] = issueCount
			c["repositoryCount"] = repoCount
			return c, true
		}
	case "Mutation":
		input := inputOf(a)
		switch field {
		case "createIssue":
			r, isRepo := nodeByID(input["repositoryId"]).(*Repo)
			if !isRepo {
				notResolvable(input, "repositoryId")
			}
			var labelNames []any
			if arr, isArr := input["labelIds"].([]any); isArr {
				for _, id := range arr {
					var found *Label
					for _, l := range r.labelOrder {
						if globalID("LA", l.ID) == id {
							found = l
							break
						}
					}
					if found == nil {
						gfail("Could not resolve to a node with the global id of '"+jsString(id, true)+"'", "NOT_FOUND")
					}
					labelNames = append(labelNames, found.Name)
				}
			}
			t, tok := input["title"]
			var labels any = labelNames
			if labelNames == nil {
				labels = []any{}
			}
			i := createIssueChecked(r, t, tok, input["body"], labels, true)
			return map[string]any{"issue": i, "clientMutationId": cmid(input)}, true
		case "updateIssue":
			i, isI := nodeByID(input["id"]).(*Issue)
			if !isI {
				notResolvable(input, "id")
			}
			if s, isStr := input["title"].(string); isStr {
				i.Title = s
			}
			if s, isStr := input["body"].(string); isStr {
				i.Body = s
			}
			if st := input["state"]; st == "OPEN" || st == "CLOSED" {
				if st == "OPEN" {
					setIssueState(i, "open")
				} else {
					setIssueState(i, "closed")
				}
			}
			i.dirty()
			return map[string]any{"issue": i, "clientMutationId": cmid(input)}, true
		case "closeIssue", "reopenIssue":
			i, isI := nodeByID(input["issueId"]).(*Issue)
			if !isI {
				notResolvable(input, "issueId")
			}
			if field == "closeIssue" {
				setIssueState(i, "closed")
			} else {
				setIssueState(i, "open")
			}
			return map[string]any{"issue": i, "clientMutationId": cmid(input)}, true
		case "addComment":
			subject := nodeByID(input["subjectId"])
			var issue *Issue
			switch s := subject.(type) {
			case *Issue:
				issue = s
			case *Pull:
				issue = s.Issue
			default:
				notResolvable(input, "subjectId")
			}
			body, isStr := input["body"].(string)
			if !isStr || body == "" {
				gfail("Body cannot be blank", "UNPROCESSABLE")
			}
			c := addComment(issue, body)
			edge := map[string]any{"__t": "IssueCommentEdge", "cursor": cursorOf(float64(len(issue.Comments) - 1)), "node": c}
			return map[string]any{"commentEdge": edge, "timelineEdge": edge, "subject": subject, "clientMutationId": cmid(input)}, true
		case "createPullRequest":
			r, isRepo := nodeByID(input["repositoryId"]).(*Repo)
			if !isRepo {
				notResolvable(input, "repositoryId")
			}
			return map[string]any{"pullRequest": createPullChecked(r, input["title"], input["body"], input["headRefName"], input["baseRefName"]), "clientMutationId": cmid(input)}, true
		case "mergePullRequest":
			p, isP := nodeByID(input["pullRequestId"]).(*Pull)
			if !isP {
				notResolvable(input, "pullRequestId")
			}
			method := "merge"
			if s, isStr := input["mergeMethod"].(string); isStr {
				method = strings.ToLower(s)
			}
			_, status, msg := mergePull(p, method, strOrNil(input["commitHeadline"]), strOrNil(input["commitBody"]))
			if status != 0 {
				gfail(msg, "UNPROCESSABLE")
			}
			return map[string]any{"pullRequest": p, "clientMutationId": cmid(input)}, true
		case "closePullRequest":
			p, isP := nodeByID(input["pullRequestId"]).(*Pull)
			if !isP {
				notResolvable(input, "pullRequestId")
			}
			if !p.Merged {
				setIssueState(p.Issue, "closed")
			}
			return map[string]any{"pullRequest": p, "clientMutationId": cmid(input)}, true
		case "createRepository":
			r := createRepoChecked(input["name"], input["description"], input["visibility"] == "PRIVATE", false)
			return map[string]any{"repository": r, "clientMutationId": cmid(input)}, true
		}
	case "User":
		switch field {
		case "id":
			return globalID("U", VIEWER.ID), true
		case "databaseId":
			return VIEWER.ID, true
		case "login":
			return VIEWER.Login, true
		case "name":
			return VIEWER.Name, true
		case "email":
			return VIEWER.Email, true
		case "url":
			return gqlOrigin + "/" + VIEWER.Login, true
		case "avatarUrl":
			return gqlOrigin + "/" + VIEWER.Login + ".png", true
		case "repositories":
			rs := listRepos(VIEWER.Login)
			items := make([]any, len(rs))
			for k, r := range rs {
				items[k] = r
			}
			return connection(items, a), true
		}
	case "Repository":
		r := parent.(*Repo)
		switch field {
		case "id":
			return globalID("R", r.ID), true
		case "databaseId":
			return r.ID, true
		case "name":
			return r.Name, true
		case "nameWithOwner":
			return r.Owner + "/" + r.Name, true
		case "owner":
			return viewerNode, true
		case "description":
			return r.Description, true
		case "url":
			return repoURL(r), true
		case "sshUrl":
			return "git@localhost:" + r.Owner + "/" + r.Name + ".git", true
		case "isPrivate":
			return r.Private, true
		case "visibility":
			if r.Private {
				return "PRIVATE", true
			}
			return "PUBLIC", true
		case "isFork", "isArchived", "hasWikiEnabled", "hasProjectsEnabled", "rebaseMergeAllowed":
			return false, true
		case "isEmpty":
			return len(r.Refs) == 0, true
		case "createdAt":
			return r.Created, true
		case "updatedAt":
			return r.Updated, true
		case "pushedAt":
			return r.Pushed, true
		case "defaultBranchRef":
			full := "refs/heads/" + r.DefaultBranch
			if sha, has := r.Refs[full]; has {
				return &refNode{r, full, sha}, true
			}
			return nil, true
		case "viewerPermission":
			return "ADMIN", true
		case "hasIssuesEnabled", "viewerCanAdminister", "mergeCommitAllowed", "squashMergeAllowed":
			return true, true
		case "issue":
			i := issueByNumber(r, a["number"])
			if i == nil || i.Pull != nil {
				v, has := a["number"]
				gfail("Could not resolve to an Issue with the number of "+jsString(v, has)+".", "NOT_FOUND")
			}
			return i, true
		case "issueOrPullRequest":
			i := issueByNumber(r, a["number"])
			if i == nil {
				v, has := a["number"]
				gfail("Could not resolve to an issue or pull request with the number of "+jsString(v, has)+".", "NOT_FOUND")
			}
			if i.Pull != nil {
				return i.Pull, true
			}
			return i, true
		case "issues":
			states := issueStatesArg(a)
			labels, has := a["labels"]
			if !has || labels == nil {
				if fb, isM := a["filterBy"].(map[string]any); isM {
					labels = fb["labels"]
				}
			}
			larr, isArr := labels.([]any)
			var items []*Issue
			for _, i := range r.Issues {
				if i.Pull != nil {
					continue
				}
				if states != nil && !states[strings.ToUpper(i.State)] {
					continue
				}
				if isArr {
					okAll := true
					for _, l := range larr {
						found := false
						for _, x := range i.Labels {
							if x.Name == l {
								found = true
								break
							}
						}
						if !found {
							okAll = false
							break
						}
					}
					if !okAll {
						continue
					}
				}
				items = append(items, i)
			}
			return connection(issuesToAny(sortIssues(items, a["orderBy"], "ASC")), a), true
		case "pullRequest":
			i := issueByNumber(r, a["number"])
			if i == nil || i.Pull == nil {
				v, has := a["number"]
				gfail("Could not resolve to a PullRequest with the number of "+jsString(v, has)+".", "NOT_FOUND")
			}
			return i.Pull, true
		case "pullRequests":
			var states map[string]bool
			if arr, isArr := a["states"].([]any); isArr && len(arr) > 0 {
				states = map[string]bool{}
				for _, s := range arr {
					states[jsString(s, true)] = true
				}
			}
			head, hasHead := a["headRefName"].(string)
			base, hasBase := a["baseRefName"].(string)
			var items []*Issue
			for _, i := range r.Issues {
				if i.Pull == nil {
					continue
				}
				if states != nil && !states[pullState(i.Pull)] {
					continue
				}
				if hasHead && i.Pull.Head != head {
					continue
				}
				if hasBase && i.Pull.Base != base {
					continue
				}
				items = append(items, i)
			}
			sorted := sortIssues(items, a["orderBy"], "ASC")
			out := make([]any, len(sorted))
			for k, i := range sorted {
				out[k] = i.Pull
			}
			return connection(out, a), true
		case "ref":
			q := ""
			if v, has := a["qualifiedName"]; has && v != nil {
				q = jsString(v, true)
			}
			full := q
			if !strings.HasPrefix(q, "refs/") {
				full = "refs/heads/" + q
			}
			if sha, has := r.Refs[full]; has {
				return &refNode{r, full, sha}, true
			}
			return nil, true
		case "refs":
			prefix := "refs/heads/"
			if v, has := a["refPrefix"]; has && v != nil {
				prefix = jsString(v, true)
			}
			items := []any{}
			for _, e := range sortedRefEntriesJS(r) {
				if strings.HasPrefix(e[0], prefix) {
					items = append(items, &refNode{r, e[0], e[1]})
				}
			}
			return connection(items, a), true
		case "label":
			n := ""
			if v, has := a["name"]; has && v != nil {
				n = jsString(v, true)
			}
			if l := r.Labels[strings.ToLower(n)]; l != nil {
				return l, true
			}
			return nil, true
		case "labels":
			return connection(labelsToAny(r.labelOrder), a), true
		}
	case "Issue":
		i := parent.(*Issue)
		switch field {
		case "id":
			return globalID("I", i.ID), true
		case "databaseId":
			return i.ID, true
		case "number":
			return i.Number, true
		case "title":
			return i.Title, true
		case "body", "bodyText":
			return i.Body, true
		case "state":
			return strings.ToUpper(i.State), true
		case "stateReason":
			if i.State == "closed" {
				return "COMPLETED", true
			}
			return nil, true
		case "url":
			return repoURL(i.Repo) + "/issues/" + strconv.Itoa(i.Number), true
		case "createdAt":
			return i.Created, true
		case "updatedAt":
			return i.Updated, true
		case "closed":
			return i.State == "closed", true
		case "closedAt":
			return strPtr(i.Closed), true
		case "author":
			return viewerNode, true
		case "repository":
			return i.Repo, true
		case "labels":
			return connection(labelsToAny(i.Labels), a), true
		case "comments":
			return connection(commentsToAny(i.Comments), a), true
		case "viewerCanUpdate":
			return true, true
		case "locked":
			return false, true
		}
	case "PullRequest":
		p := parent.(*Pull)
		switch field {
		case "id":
			return globalID("PR", p.ID), true
		case "databaseId":
			return p.ID, true
		case "number":
			return p.Issue.Number, true
		case "title":
			return p.Issue.Title, true
		case "body":
			return p.Issue.Body, true
		case "state":
			return pullState(p), true
		case "url":
			return repoURL(p.Issue.Repo) + "/pull/" + strconv.Itoa(p.Issue.Number), true
		case "createdAt":
			return p.Issue.Created, true
		case "updatedAt":
			return p.Issue.Updated, true
		case "closed":
			return p.Issue.State == "closed", true
		case "closedAt":
			return strPtr(p.Issue.Closed), true
		case "merged":
			return p.Merged, true
		case "mergedAt":
			return strPtr(p.MergedAt), true
		case "mergeable":
			if p.Issue.State != "open" {
				return "UNKNOWN", true
			}
			if isMergeable(p) {
				return "MERGEABLE", true
			}
			return "CONFLICTING", true
		case "headRefName":
			return p.Head, true
		case "baseRefName":
			return p.Base, true
		case "headRefOid":
			h, _ := pullShas(p)
			return h, true
		case "baseRefOid":
			_, b := pullShas(p)
			return b, true
		case "isDraft", "locked":
			return false, true
		case "author":
			return viewerNode, true
		case "repository", "headRepository", "baseRepository":
			return p.Issue.Repo, true
		case "mergeCommit":
			if p.MergeSha == nil {
				return nil, true
			}
			return &commitNode{p.Issue.Repo, *p.MergeSha}, true
		case "additions", "deletions", "changedFiles":
			files := pullFiles(p)
			n := 0
			for _, f := range files {
				if field == "additions" {
					n += f.Additions
				} else if field == "deletions" {
					n += f.Deletions
				}
			}
			if field == "changedFiles" {
				n = len(files)
			}
			return n, true
		case "files":
			files := pullFiles(p)
			items := make([]any, len(files))
			for k, f := range files {
				ct := "MODIFIED"
				if f.Status == "added" {
					ct = "ADDED"
				} else if f.Status == "removed" {
					ct = "DELETED"
				}
				items[k] = map[string]any{"__t": "PullRequestChangedFile", "path": f.Path, "additions": f.Additions, "deletions": f.Deletions, "changeType": ct}
			}
			return connection(items, a), true
		case "labels":
			return connection(labelsToAny(p.Issue.Labels), a), true
		case "comments":
			return connection(commentsToAny(p.Issue.Comments), a), true
		}
	case "IssueComment":
		c := parent.(*Comment)
		switch field {
		case "id":
			return globalID("IC", c.ID), true
		case "databaseId":
			return c.ID, true
		case "body":
			return c.Body, true
		case "author":
			return viewerNode, true
		case "createdAt":
			return c.Created, true
		case "url":
			return repoURL(c.Issue.Repo) + "/issues/" + strconv.Itoa(c.Issue.Number) + "#issuecomment-" + strconv.Itoa(c.ID), true
		case "issue":
			return c.Issue, true
		}
	case "Label":
		l := parent.(*Label)
		switch field {
		case "id":
			return globalID("LA", l.ID), true
		case "name":
			return l.Name, true
		case "color":
			return l.Color, true
		case "description":
			return l.Description, true
		}
	case "Ref":
		x := parent.(*refNode)
		switch field {
		case "id":
			return "REF_" + b64url(strconv.Itoa(x.repo.ID)+":"+x.refName), true
		case "name":
			return refPrefixRe.ReplaceAllString(x.refName, ""), true
		case "prefix":
			if m := refPrefixRe.FindString(x.refName); m != "" {
				return m, true
			}
			return "refs/", true
		case "target":
			return &commitNode{x.repo, x.sha}, true
		case "repository":
			return x.repo, true
		}
	case "Commit":
		x := parent.(*commitNode)
		msg := func() string {
			if c := commitOf(x.repo, x.sha); c != nil {
				return c.Message
			}
			return ""
		}
		switch field {
		case "id":
			raw, _ := hex.DecodeString(x.sha)
			return "C_" + base64.RawURLEncoding.EncodeToString(raw), true
		case "oid":
			return x.sha, true
		case "message":
			return strings.TrimSuffix(msg(), "\n"), true
		case "messageHeadline":
			return strings.SplitN(msg(), "\n", 2)[0], true
		case "committedDate":
			return "2026-01-01T00:00:00Z", true
		case "url":
			return repoURL(x.repo) + "/commit/" + x.sha, true
		}
	}
	return nil, false
}

// ------------------------------------------------------------------ executor

type execCtx struct {
	doc     *document
	vars    map[string]any
	errors  []any
	invalid bool
}

func (ex *execCtx) addError(e O) { ex.errors = append(ex.errors, e) }

func valueOf(v *gValue, ex *execCtx) any {
	switch v.kind {
	case "var":
		return ex.vars[v.name]
	case "list":
		out := make([]any, len(v.items))
		for k, x := range v.items {
			out[k] = valueOf(x, ex)
		}
		return out
	case "object":
		o := map[string]any{}
		for _, f := range v.fields {
			o[f.name] = valueOf(f.v, ex)
		}
		return o
	case "null":
		return nil
	}
	return v.val
}

func namedType(ref string) string {
	return strings.NewReplacer("[", "", "]", "", "!", "").Replace(ref)
}

type fieldGroups struct {
	keys []string
	m    map[string][]*selection
}

func (g *fieldGroups) add(k string, s *selection) {
	if _, has := g.m[k]; !has {
		g.keys = append(g.keys, k)
	}
	g.m[k] = append(g.m[k], s)
}

func responseKey(s *selection) string {
	if s.alias != "" {
		return s.alias
	}
	return s.name
}

func typeMatches(concrete, cond string) bool {
	if concrete == cond {
		return true
	}
	d := types[cond]
	if d == nil {
		return false
	}
	if d.kind == "UNION" {
		for _, m := range d.members {
			if m == concrete {
				return true
			}
		}
		return false
	}
	if d.kind == "INTERFACE" {
		if c := types[concrete]; c != nil {
			for _, i := range c.interfaces {
				if i == cond {
					return true
				}
			}
		}
	}
	return false
}

func collect(ex *execCtx, typeName string, sel []*selection, out *fieldGroups, visited map[string]bool) {
	for _, s := range sel {
		switch s.kind {
		case 'f':
			out.add(responseKey(s), s)
		case 'i':
			if !s.hasOn || typeMatches(typeName, s.on) {
				collect(ex, typeName, s.sel, out, visited)
			}
		default:
			f := ex.doc.frags[s.name]
			if f == nil {
				ex.addError(O{{"message", "Fragment " + s.name + " was used, but not defined"}})
				ex.invalid = true
				continue
			}
			if visited[s.name] {
				continue
			}
			visited[s.name] = true
			if typeMatches(typeName, f.on) {
				collect(ex, typeName, f.sel, out, visited)
			}
		}
	}
}

func fieldDefOf(typeName, field string) (string, bool) {
	if intro, has := introspectionTypes[typeName]; has {
		t, ok := intro[field]
		return t, ok
	}
	d := types[typeName]
	if d == nil {
		return "", false
	}
	f := d.fields[field]
	if f == nil {
		return "", false
	}
	return f.typ, true
}

func pathCopy(path []any, k any) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, k)
}

func selectObject(ex *execCtx, typeName string, value any, sel []*selection, path []any) O {
	g := &fieldGroups{m: map[string][]*selection{}}
	collect(ex, typeName, sel, g, map[string]bool{})
	out := make(O, 0, len(g.keys))
	for _, key := range g.keys {
		list := g.m[key]
		f := list[0]
		var sub []*selection
		any := false
		for _, x := range list {
			if x.hasSel {
				any = true
				sub = append(sub, x.sel...)
			}
		}
		if len(sub) == 0 {
			sub = nil
		}
		_ = any
		out = append(out, KV{key, resolveField(ex, typeName, value, f, sub, pathCopy(path, key))})
	}
	return out
}

func evalArgs(ex *execCtx, f *selection) map[string]any {
	args := map[string]any{}
	for _, a := range f.args {
		args[a.name] = valueOf(a.v, ex)
	}
	return args
}

func resolveField(ex *execCtx, typeName string, value any, f *selection, sel []*selection, path []any) (result any) {
	if f.name == "__typename" {
		return typeName
	}
	if typeName == "Query" && f.name == "__type" {
		args := evalArgs(ex, f)
		name := ""
		if v, has := args["name"]; has && v != nil {
			name = jsString(v, true)
		}
		var t *gType
		if types[name] != nil {
			t = typeRefObj(name)
		} else if introspectionTypes[name] != nil {
			n := name
			t = &gType{kind: "OBJECT", name: &n}
		}
		if t == nil || sel == nil {
			return nil
		}
		return selectObject(ex, "__Type", t, sel, path)
	}
	if typeName == "Query" && f.name == "__schema" {
		if sel == nil {
			return nil
		}
		return selectObject(ex, "__Schema", map[string]any{}, sel, path)
	}
	ftype, okd := fieldDefOf(typeName, f.name)
	if !okd {
		var sp []any
		for _, p := range path {
			if s, isStr := p.(string); isStr {
				sp = append(sp, s)
			}
		}
		ex.addError(O{{"message", "Field '" + f.name + "' doesn't exist on type '" + typeName + "'"}, {"type", "undefinedField"}, {"path", sp}})
		ex.invalid = true
		return nil
	}
	args := evalArgs(ex, f)
	raw, failed := callResolver(ex, typeName, f.name, value, args, path)
	if failed {
		return nil
	}
	return complete(ex, ftype, raw, sel, path, f.name)
}

func callResolver(ex *execCtx, typeName, field string, value any, args map[string]any, path []any) (raw any, failed bool) {
	defer func() {
		if p := recover(); p != nil {
			msg, typ := "", "INTERNAL"
			switch e := p.(type) {
			case *gqlError:
				msg, typ = e.msg, e.typ
			case *HttpError:
				msg, typ = e.Message, "UNPROCESSABLE"
			default:
				msg = panicMessage(p)
			}
			ex.addError(O{{"message", msg}, {"type", typ}, {"path", path}})
			raw, failed = nil, true
		}
	}()
	if strings.HasPrefix(typeName, "__") {
		return introspect(typeName, field, value), false
	}
	if r, found := resolve(typeName, field, value, args); found {
		return r, false
	}
	if m, isM := value.(map[string]any); isM {
		return m[field], false
	}
	return nil, false
}

func isLeafType(name string) bool {
	if name == "__TypeKind" {
		return true
	}
	if _, intro := introspectionTypes[name]; intro {
		return false
	}
	d := types[name]
	return d == nil || d.kind == "SCALAR" || d.kind == "ENUM"
}

func complete(ex *execCtx, ref string, raw any, sel []*selection, path []any, fieldName string) any {
	if raw == nil {
		return nil
	}
	switch x := raw.(type) {
	case *Repo:
		if x == nil {
			return nil
		}
	case *Issue:
		if x == nil {
			return nil
		}
	case *Pull:
		if x == nil {
			return nil
		}
	}
	inner := strings.TrimSuffix(ref, "!")
	if strings.HasPrefix(inner, "[") {
		arr, isArr := raw.([]any)
		if !isArr {
			return nil
		}
		itemRef := inner[1 : len(inner)-1]
		out := make([]any, len(arr))
		for k, item := range arr {
			out[k] = complete(ex, itemRef, item, sel, pathCopy(path, k), fieldName)
		}
		return out
	}
	name := namedType(inner)
	if isLeafType(name) {
		if sel != nil {
			names := make([]string, len(sel))
			for k, s := range sel {
				if s.kind == 'f' {
					names[k] = s.name
				} else {
					names[k] = "..."
				}
			}
			ex.addError(O{{"message", "Selections can't be made on scalars (field '" + fieldName + "' returns " + name + " but has selections [" + strings.Join(names, ", ") + "])"}})
			ex.invalid = true
		}
		return raw
	}
	if sel == nil {
		ex.addError(O{{"message", "Field must have selections (field '" + fieldName + "' returns " + name + " but has no selections. Did you mean '" + fieldName + " { ... }'?)"}})
		ex.invalid = true
		return nil
	}
	concrete := name
	d := types[name]
	_, intro := introspectionTypes[name]
	if d != nil && (d.kind == "INTERFACE" || d.kind == "UNION") {
		concrete = typeOfValue(raw)
	} else if d == nil && !intro {
		concrete = typeOfValue(raw)
	}
	return selectObject(ex, concrete, raw, sel, path)
}

func executeGraphql(reqv any, origin string) O {
	gqlOrigin = origin
	req, isObj := reqv.(map[string]any)
	q, isStr := req["query"].(string)
	if !isObj || !isStr {
		return O{{"errors", []any{O{{"message", "A query attribute must be specified and must be a string."}}}}}
	}
	doc, err := parseGql(q)
	if err != nil {
		return O{{"errors", []any{O{{"message", "Parse error on " + err.Error()}, {"type", "PARSE"}}}}}
	}
	opName, _ := req["operationName"].(string)
	var op *operation
	if opName == "" {
		if len(doc.ops) == 1 {
			op = doc.ops[0]
		}
	} else {
		for _, o := range doc.ops {
			if o.hasName && o.name == opName {
				op = o
				break
			}
		}
	}
	if op == nil {
		msg := "An operation name is required when the document holds more than one operation"
		if opName != "" {
			msg = "No operation named \"" + opName + "\""
		}
		return O{{"errors", []any{O{{"message", msg}}}}}
	}
	given, _ := req["variables"].(map[string]any)
	vars := map[string]any{}
	for _, v := range op.vars {
		if gv, has := given[v.name]; has {
			vars[v.name] = gv
		} else if v.def != nil {
			vars[v.name] = valueOf(v.def, &execCtx{doc: doc, vars: map[string]any{}})
		}
	}
	ex := &execCtx{doc: doc, vars: vars}
	root := "Query"
	if op.op == "mutation" {
		root = "Mutation"
	}
	validate(ex, root, op.sel)
	if ex.invalid {
		return O{{"errors", ex.errors}}
	}
	data := selectObject(ex, root, map[string]any{}, op.sel, []any{})
	if len(ex.errors) > 0 {
		return O{{"data", data}, {"errors", ex.errors}}
	}
	return O{{"data", data}}
}

func validate(ex *execCtx, typeName string, sel []*selection) {
	g := &fieldGroups{m: map[string][]*selection{}}
	collectStatic(ex, typeName, sel, g)
	for _, k := range g.keys {
		for _, f := range g.m[k] {
			if f.name == "__typename" || f.name == "__type" || f.name == "__schema" {
				continue
			}
			ftype, okd := fieldDefOf(typeName, f.name)
			if !okd {
				ex.addError(O{{"message", "Field '" + f.name + "' doesn't exist on type '" + typeName + "'"}, {"type", "undefinedField"}})
				ex.invalid = true
				continue
			}
			name := namedType(ftype)
			_, intro := introspectionTypes[name]
			td := types[name]
			leaf := !intro && (td == nil || td.kind == "SCALAR" || td.kind == "ENUM")
			if leaf && f.hasSel {
				ex.addError(O{{"message", "Selections can't be made on scalars (field '" + f.name + "' returns " + name + ")"}})
				ex.invalid = true
			} else if !leaf && !f.hasSel {
				ex.addError(O{{"message", "Field must have selections (field '" + f.name + "' returns " + name + " but has no selections)"}})
				ex.invalid = true
			} else if !leaf && f.hasSel && !intro {
				validate(ex, name, f.sel)
			}
		}
	}
}

func collectStatic(ex *execCtx, typeName string, sel []*selection, out *fieldGroups) {
	for _, s := range sel {
		switch s.kind {
		case 'f':
			out.add(responseKey(s), s)
		case 'i':
			if !s.hasOn || s.on == typeName {
				collectStatic(ex, typeName, s.sel, out)
			} else if types[s.on] != nil {
				validate(ex, s.on, s.sel)
			} else {
				ex.addError(O{{"message", "No such type " + s.on + ", so it can't be a fragment condition"}})
				ex.invalid = true
			}
		default:
			f := ex.doc.frags[s.name]
			if f == nil {
				ex.addError(O{{"message", "Fragment " + s.name + " was used, but not defined"}})
				ex.invalid = true
			} else if f.on == typeName {
				collectStatic(ex, typeName, f.sel, out)
			} else if types[f.on] != nil {
				validate(ex, f.on, f.sel)
			}
		}
	}
}
