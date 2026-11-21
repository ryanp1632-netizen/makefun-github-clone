// The forge's whole state, in memory: repositories with their git objects and refs, and the issues,
// labels, comments and pull requests around them. Every surface reads and writes through here.

package main

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ identity

// Person is a scenario user.
type Person struct {
	Login, Name, Email string
	ID                 int
}

var firstNames = []string{"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane"}
var lastNames = []string{"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer"}

const seed uint64 = 0x6a09e667f3bcc909

func fnv1a(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type rngStream struct{ state uint64 }

func newStream(label string) *rngStream {
	s := mix64(seed ^ fnv1a(label))
	if s == 0 {
		s = seed
	}
	return &rngStream{s}
}

func (r *rngStream) u32() uint32 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return uint32((x * 0x2545f4914f6cdd1d) >> 32)
}

func (r *rngStream) intn(max int) int {
	if max == 1 {
		return 0
	}
	const rng = uint64(4294967296)
	limit := rng - rng%uint64(max)
	for {
		v := uint64(r.u32())
		if v < limit {
			return int(v % uint64(max))
		}
	}
}

var nonLogin = regexp.MustCompile(`[^a-z0-9-]`)

func viewerPerson() Person {
	if l := os.Getenv("FORGE_LOGIN"); l != "" {
		return Person{Login: l, Name: l, Email: l + "@example.invalid", ID: 1000}
	}
	rng := newStream("users")
	first := firstNames[rng.intn(len(firstNames))]
	last := lastNames[rng.intn(len(lastNames))]
	login := nonLogin.ReplaceAllString(first+last, "")
	return Person{Login: login, Name: first + " " + last, Email: login + "@example.invalid", ID: 1000}
}

var VIEWER = viewerPerson()

// ------------------------------------------------------------------ the logical clock

const epochSec int64 = 1767225600 // 2026-01-01T00:00:00Z

var tick int64

func nextTime() (string, int64) {
	tick++
	sec := epochSec + tick
	return isoOfSec(sec), sec
}

func nextISO() string { s, _ := nextTime(); return s }

var idCounter int

func nextID() int {
	idCounter++
	return 100000 + idCounter
}

func globalID(kind string, id int) string {
	return kind + "_" + b64url(kind+":"+strconv.Itoa(id))
}

var gidRe = regexp.MustCompile(`^([A-Za-z]+):(\d+)$`)

func decodeGlobalID(gid string) (string, int, bool) {
	us := strings.IndexByte(gid, '_')
	if us < 1 {
		return "", 0, false
	}
	text := toValidUTF8(lenientB64(gid[us+1:]))
	m := gidRe.FindStringSubmatch(text)
	if m == nil || m[1] != gid[:us] {
		return "", 0, false
	}
	n, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return "", 0, false
	}
	return m[1], int(n), true
}

// ------------------------------------------------------------------ the model

type Label struct {
	ID                       int
	Name, Color, Description string
}

type Comment struct {
	ID      int
	Body    string
	Created string
	Issue   *Issue
}

type Issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string
	Labels   []*Label
	Comments []*Comment
	Created  string
	Updated  string
	Closed   *string
	Seq      int
	Pull     *Pull
	Repo     *Repo
	json     []byte
	jsonOrig string
	wire     []byte
	wireJSON []byte
}

func (i *Issue) dirty() { i.json = nil }

type Pull struct {
	Issue    *Issue
	ID       int
	Head     string
	Base     string
	HeadSha  string
	BaseSha  string
	Merged   bool
	MergedAt *string
	MergeSha *string
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Created       string
	Updated       string
	Pushed        string
	Objects       map[string]*Obj
	Refs          map[string]string
	refOrder      []string
	RefVersion    int
	packCache     map[string][]byte
	Labels        map[string]*Label
	labelOrder    []*Label
	Issues        []*Issue
	ByNumber      map[int]*Issue
	NextNumber    int
	respVersion   int
	respCache     map[string][]byte
	planVersion   int
	planCache     map[string]*fetchResult
}

var repos = map[string]*Repo{}
var repoList []*Repo
var reposByID = map[int]*Repo{}
var issuesByID = map[int]*Issue{}
var commentsByID = map[int]*Comment{}
var pullsByID = map[int]*Pull{}

func repoKey(owner, name string) string { return strings.ToLower(owner) + "/" + strings.ToLower(name) }

func getRepo(owner, name string) *Repo { return repos[repoKey(owner, name)] }

func listRepos(owner string) []*Repo {
	var out []*Repo
	lo := strings.ToLower(owner)
	for _, r := range repoList {
		if strings.ToLower(r.Owner) == lo {
			out = append(out, r)
		}
	}
	return out
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func validRepoName(v any) (string, bool) {
	name, ok := v.(string)
	if !ok {
		return "", false
	}
	return name, repoNameRe.MatchString(name) && name != "." && name != ".." && !strings.HasSuffix(name, ".git")
}

func createRepo(name, description string, private, autoInit bool) *Repo {
	t := nextISO()
	id := nextID()
	r := &Repo{
		ID: id, Owner: VIEWER.Login, Name: name, Description: description, Private: private, DefaultBranch: "main",
		Created: t, Updated: t, Pushed: t, Objects: map[string]*Obj{}, Refs: map[string]string{},
		packCache: map[string][]byte{}, Labels: map[string]*Label{}, ByNumber: map[int]*Issue{}, NextNumber: 1,
	}
	k := repoKey(VIEWER.Login, name)
	if old, ok := repos[k]; ok {
		removeRepoFromList(old)
	}
	repos[k] = r
	repoList = append(repoList, r)
	reposByID[id] = r
	if autoInit {
		commitFiles(r, "main", []fileChange{{Path: "README.md", Content: []byte("# " + name + "\n")}}, "Initial commit")
	}
	return r
}

func removeRepoFromList(r *Repo) {
	for i, x := range repoList {
		if x == r {
			repoList = append(repoList[:i:i], repoList[i+1:]...)
			return
		}
	}
}

func deleteRepo(r *Repo) {
	delete(repos, repoKey(r.Owner, r.Name))
	removeRepoFromList(r)
	delete(reposByID, r.ID)
	for _, i := range r.Issues {
		delete(issuesByID, i.ID)
		if i.Pull != nil {
			delete(pullsByID, i.Pull.ID)
		}
		for _, c := range i.Comments {
			delete(commentsByID, c.ID)
		}
	}
}

func touch(r *Repo) { r.Updated = nextISO() }

// ------------------------------------------------------------------ objects and refs

func putObject(r *Repo, typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Obj{Type: typ, Data: data}
	}
	return oid
}

func setRef(r *Repo, ref string, oid *string) {
	if oid == nil {
		if _, ok := r.Refs[ref]; ok {
			delete(r.Refs, ref)
			for i, n := range r.refOrder {
				if n == ref {
					r.refOrder = append(r.refOrder[:i:i], r.refOrder[i+1:]...)
					break
				}
			}
		}
	} else {
		if _, ok := r.Refs[ref]; !ok {
			r.refOrder = append(r.refOrder, ref)
		}
		r.Refs[ref] = *oid
	}
	r.RefVersion++
	r.packCache = map[string][]byte{}
	r.Pushed = nextISO()
}

func setRefTo(r *Repo, ref, oid string) { setRef(r, ref, &oid) }

// refEntries returns [name, sha] pairs in insertion order.
func refEntries(r *Repo) [][2]string {
	out := make([][2]string, 0, len(r.refOrder))
	for _, n := range r.refOrder {
		out = append(out, [2]string{n, r.Refs[n]})
	}
	return out
}

// sortedRefEntriesJS sorts like JS's default Array#sort on [name, sha] pairs ("name,sha" strings).
func sortedRefEntriesJS(r *Repo) [][2]string {
	e := refEntries(r)
	sort.SliceStable(e, func(a, b int) bool { return e[a][0]+","+e[a][1] < e[b][0]+","+e[b][1] })
	return e
}

func resolveRev(r *Repo, rev string) (string, bool) {
	if rev == "" {
		s, ok := r.Refs["refs/heads/"+r.DefaultBranch]
		return s, ok
	}
	if d, ok := r.Refs[rev]; ok {
		return peelToCommit(r, d)
	}
	if d, ok := r.Refs["refs/heads/"+rev]; ok {
		return peelToCommit(r, d)
	}
	if d, ok := r.Refs["refs/tags/"+rev]; ok {
		return peelToCommit(r, d)
	}
	if isOid(rev) {
		if _, ok := r.Objects[rev]; ok {
			return peelToCommit(r, rev)
		}
	}
	return "", false
}

func peelToCommit(r *Repo, oid string) (string, bool) {
	cur := oid
	for i := 0; i < 10; i++ {
		o := r.Objects[cur]
		if o == nil {
			return "", false
		}
		if o.Type == "commit" {
			return cur, true
		}
		if o.Type != "tag" {
			return "", false
		}
		cur, _ = parseTagTarget(o.Data)
	}
	return "", false
}

func commitOf(r *Repo, oid string) *CommitInfo {
	o := r.Objects[oid]
	if o == nil || o.Type != "commit" {
		return nil
	}
	if o.commit == nil {
		o.commit = parseCommit(o.Data)
	}
	return o.commit
}

func isAncestor(r *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if c := commitOf(r, cur); c != nil {
			stack = append(stack, c.Parents...)
		}
	}
	return false
}

func mergeBase(r *Repo, a, b string) (string, bool) {
	ancestors := map[string]bool{}
	stack := []string{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[cur] {
			continue
		}
		ancestors[cur] = true
		if c := commitOf(r, cur); c != nil {
			stack = append(stack, c.Parents...)
		}
	}
	queue := []string{b}
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if ancestors[cur] {
			return cur, true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if c := commitOf(r, cur); c != nil {
			queue = append(queue, c.Parents...)
		}
	}
	return "", false
}

func history(r *Repo, tip string, limit int) []string {
	var out []string
	seen := map[string]bool{tip: true}
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		cur := queue[0]
		queue = queue[1:]
		out = append(out, cur)
		c := commitOf(r, cur)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ trees as flat path maps

type FileEntry struct{ Mode, Oid string }

// fileMap is an insertion-ordered path -> entry map (JS Map semantics).
type fileMap struct {
	keys []string
	m    map[string]FileEntry
}

func newFileMap() *fileMap { return &fileMap{m: map[string]FileEntry{}} }

func (f *fileMap) get(k string) (FileEntry, bool) { e, ok := f.m[k]; return e, ok }

func (f *fileMap) set(k string, e FileEntry) {
	if _, ok := f.m[k]; !ok {
		f.keys = append(f.keys, k)
	}
	f.m[k] = e
}

func (f *fileMap) del(k string) {
	if _, ok := f.m[k]; !ok {
		return
	}
	delete(f.m, k)
	for i, x := range f.keys {
		if x == k {
			f.keys = append(f.keys[:i:i], f.keys[i+1:]...)
			return
		}
	}
}

func flattenTree(r *Repo, treeOid, prefix string, out *fileMap) *fileMap {
	o := r.Objects[treeOid]
	if o == nil || o.Type != "tree" {
		return out
	}
	for _, e := range mustTree(o.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTree(r, e.Oid, path, out)
		} else {
			out.set(path, FileEntry{e.Mode, e.Oid})
		}
	}
	return out
}

type dirNode struct {
	files     []string
	fileM     map[string]FileEntry
	dirs      []string
	dirM      map[string]*dirNode
}

func newDir() *dirNode { return &dirNode{fileM: map[string]FileEntry{}, dirM: map[string]*dirNode{}} }

func buildTreeFromMap(r *Repo, files *fileMap) string {
	root := newDir()
	for _, path := range files.keys {
		e := files.m[path]
		parts := strings.Split(path, "/")
		node := root
		for i := 0; i < len(parts)-1; i++ {
			next := node.dirM[parts[i]]
			if next == nil {
				next = newDir()
				node.dirM[parts[i]] = next
				node.dirs = append(node.dirs, parts[i])
			}
			node = next
		}
		name := parts[len(parts)-1]
		if _, ok := node.fileM[name]; !ok {
			node.files = append(node.files, name)
		}
		node.fileM[name] = e
	}
	var write func(d *dirNode) string
	write = func(d *dirNode) string {
		var entries []TreeEntry
		for _, n := range d.files {
			e := d.fileM[n]
			entries = append(entries, TreeEntry{e.Mode, n, e.Oid})
		}
		for _, n := range d.dirs {
			entries = append(entries, TreeEntry{"40000", n, write(d.dirM[n])})
		}
		return putObject(r, "tree", buildTree(entries))
	}
	return write(root)
}

func treeOfCommit(r *Repo, commit string, ok bool) *fileMap {
	if !ok {
		return newFileMap()
	}
	c := commitOf(r, commit)
	if c == nil {
		return newFileMap()
	}
	return flattenTree(r, c.Tree, "", newFileMap())
}

// pathResult is a file (Kind "file") or a directory listing (Kind "dir").
type pathResult struct {
	Kind    string
	Oid     string
	Mode    string
	Entries []TreeEntry
}

func lookupPath(r *Repo, commit, path string) *pathResult {
	c := commitOf(r, commit)
	if c == nil {
		return nil
	}
	treeOid := c.Tree
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		t := r.Objects[treeOid]
		if t == nil {
			return nil
		}
		return &pathResult{Kind: "dir", Oid: treeOid, Entries: mustTree(t.Data)}
	}
	for i := 0; i < len(parts); i++ {
		t := r.Objects[treeOid]
		if t == nil || t.Type != "tree" {
			return nil
		}
		var found *TreeEntry
		for _, e := range mustTree(t.Data) {
			if e.Name == parts[i] {
				e := e
				found = &e
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(parts)-1 {
			if found.Mode == "40000" {
				sub := r.Objects[found.Oid]
				if sub == nil {
					return nil
				}
				return &pathResult{Kind: "dir", Oid: found.Oid, Entries: mustTree(sub.Data)}
			}
			return &pathResult{Kind: "file", Oid: found.Oid, Mode: found.Mode}
		}
		if found.Mode != "40000" {
			return nil
		}
		treeOid = found.Oid
	}
	return nil
}

func ident() string {
	_, sec := nextTime()
	return VIEWER.Name + " <" + VIEWER.Email + "> " + strconv.FormatInt(sec, 10) + " +0000"
}

func makeCommit(r *Repo, tree string, parents []string, message string) string {
	who := ident()
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return putObject(r, "commit", buildCommit(tree, parents, who, who, message))
}

type fileChange struct {
	Path    string
	Content []byte // nil deletes
}

func commitFiles(r *Repo, branch string, changes []fileChange, message string) (string, map[string]string) {
	ref := "refs/heads/" + branch
	parent, hasParent := r.Refs[ref]
	files := treeOfCommit(r, parent, hasParent)
	blobs := map[string]string{}
	for _, ch := range changes {
		if ch.Content == nil {
			files.del(ch.Path)
		} else {
			oid := putObject(r, "blob", ch.Content)
			blobs[ch.Path] = oid
			files.set(ch.Path, FileEntry{"100644", oid})
		}
	}
	tree := buildTreeFromMap(r, files)
	var parents []string
	if hasParent {
		parents = []string{parent}
	}
	commit := makeCommit(r, tree, parents, message)
	setRefTo(r, ref, commit)
	return commit, blobs
}

// ------------------------------------------------------------------ labels, issues, comments

func addLabel(r *Repo, name, color, description string) *Label {
	l := &Label{ID: nextID(), Name: name, Color: color, Description: description}
	k := strings.ToLower(name)
	if old, ok := r.Labels[k]; ok {
		for i, x := range r.labelOrder {
			if x == old {
				r.labelOrder[i] = l
			}
		}
	} else {
		r.labelOrder = append(r.labelOrder, l)
	}
	r.Labels[k] = l
	return l
}

var issueSeq int

func createIssue(r *Repo, title, body string, labels []*Label) *Issue {
	t := nextISO()
	issueSeq++
	if labels == nil {
		labels = []*Label{}
	}
	i := &Issue{ID: nextID(), Number: r.NextNumber, Title: title, Body: body, State: "open", Labels: labels,
		Created: t, Updated: t, Seq: issueSeq, Repo: r}
	r.NextNumber++
	r.Issues = append(r.Issues, i)
	r.ByNumber[i.Number] = i
	issuesByID[i.ID] = i
	return i
}

func setIssueState(i *Issue, state string) {
	if i.State != state {
		i.State = state
		if state == "closed" {
			t := nextISO()
			i.Closed = &t
		} else {
			i.Closed = nil
		}
	}
	i.Updated = nextISO()
	i.dirty()
}

func addComment(i *Issue, body string) *Comment {
	c := &Comment{ID: nextID(), Body: body, Created: nextISO(), Issue: i}
	i.Comments = append(i.Comments, c)
	commentsByID[c.ID] = c
	i.Updated = c.Created
	i.dirty()
	return c
}

// ------------------------------------------------------------------ pull requests

func refOr(r *Repo, ref, def string) string {
	if s, ok := r.Refs[ref]; ok {
		return s
	}
	return def
}

func createPull(r *Repo, title, body, head, base string) *Pull {
	i := createIssue(r, title, body, nil)
	p := &Pull{Issue: i, ID: nextID(), Head: head, Base: base,
		HeadSha: refOr(r, "refs/heads/"+head, ZERO), BaseSha: refOr(r, "refs/heads/"+base, ZERO)}
	i.Pull = p
	pullsByID[p.ID] = p
	return p
}

func pullShas(p *Pull) (string, string) {
	if p.Issue.State == "open" {
		r := p.Issue.Repo
		p.HeadSha = refOr(r, "refs/heads/"+p.Head, p.HeadSha)
		p.BaseSha = refOr(r, "refs/heads/"+p.Base, p.BaseSha)
	}
	return p.HeadSha, p.BaseSha
}

type ChangedFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	Oid       string
}

func splitLinesJS(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func lineCounts(oldText, newText string) (int, int) {
	a := splitLinesJS(oldText)
	b := splitLinesJS(newText)
	counts := map[string]int{}
	for _, l := range a {
		counts[l]++
	}
	common := 0
	for _, l := range b {
		if counts[l] > 0 {
			counts[l]--
			common++
		}
	}
	return len(b) - common, len(a) - common
}

func pullFiles(p *Pull) []ChangedFile {
	r := p.Issue.Repo
	head, base := pullShas(p)
	mb := base
	if !p.Merged {
		if m, ok := mergeBase(r, base, head); ok {
			mb = m
		}
	}
	var beforeC, afterC string
	if p.Merged {
		beforeC = mb
		if p.MergeSha != nil {
			if c := commitOf(r, *p.MergeSha); c != nil && len(c.Parents) > 0 {
				beforeC = c.Parents[0]
			}
			afterC = *p.MergeSha
		} else {
			afterC = head
		}
	} else {
		beforeC, afterC = mb, head
	}
	before := treeOfCommit(r, beforeC, true)
	after := treeOfCommit(r, afterC, true)
	text := func(oid string) string {
		if o := r.Objects[oid]; o != nil {
			return toValidUTF8(o.Data)
		}
		return ""
	}
	var out []ChangedFile
	for _, path := range after.keys {
		e := after.m[path]
		old, ok := before.get(path)
		if !ok {
			ad, de := lineCounts("", text(e.Oid))
			out = append(out, ChangedFile{path, "added", ad, de, e.Oid})
		} else if old.Oid != e.Oid {
			ad, de := lineCounts(text(old.Oid), text(e.Oid))
			out = append(out, ChangedFile{path, "modified", ad, de, e.Oid})
		}
	}
	for _, path := range before.keys {
		if _, ok := after.get(path); !ok {
			e := before.m[path]
			ad, de := lineCounts(text(e.Oid), "")
			out = append(out, ChangedFile{path, "removed", ad, de, e.Oid})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}

func squashFiles(r *Repo, base, head string) *fileMap {
	mbc, mbok := mergeBase(r, base, head)
	m := treeOfCommit(r, mbc, mbok)
	b := treeOfCommit(r, base, true)
	h := treeOfCommit(r, head, true)
	out := newFileMap()
	seen := map[string]bool{}
	var paths []string
	for _, src := range []*fileMap{m, b, h} {
		for _, k := range src.keys {
			if !seen[k] {
				seen[k] = true
				paths = append(paths, k)
			}
		}
	}
	same := func(x FileEntry, xok bool, y FileEntry, yok bool) bool {
		if !xok {
			return !yok
		}
		return yok && x.Oid == y.Oid && x.Mode == y.Mode
	}
	for _, p := range paths {
		mv, mok := m.get(p)
		bv, bok := b.get(p)
		hv, hok := h.get(p)
		var take FileEntry
		var tok bool
		switch {
		case same(hv, hok, mv, mok):
			take, tok = bv, bok
		case same(bv, bok, mv, mok):
			take, tok = hv, hok
		case same(bv, bok, hv, hok):
			take, tok = bv, bok
		default:
			return nil
		}
		if tok {
			out.set(p, take)
		}
	}
	return out
}

func isMergeable(p *Pull) bool {
	if p.Issue.State != "open" {
		return false
	}
	head, base := pullShas(p)
	r := p.Issue.Repo
	if r.Objects[head] == nil || r.Objects[base] == nil {
		return false
	}
	return squashFiles(r, base, head) != nil
}

func mergePull(p *Pull, method string, title, message *string) (string, int, string) {
	r := p.Issue.Repo
	if p.Merged {
		return "", 405, "Pull Request is not mergeable: it is already merged"
	}
	if p.Issue.State != "open" {
		return "", 405, "Pull Request is not mergeable: it is closed"
	}
	head, base := pullShas(p)
	if r.Objects[head] == nil || r.Objects[base] == nil {
		return "", 405, "Pull Request is not mergeable"
	}
	files := squashFiles(r, base, head)
	if files == nil {
		return "", 405, "Pull Request is not mergeable: merge conflict"
	}
	tree := buildTreeFromMap(r, files)
	subject := p.Issue.Title + " (#" + strconv.Itoa(p.Issue.Number) + ")"
	if title != nil && *title != "" {
		subject = *title
	}
	body := subject
	if message != nil && *message != "" {
		body = subject + "\n\n" + *message
	}
	parents := []string{base}
	if method == "merge" {
		parents = []string{base, head}
	}
	sha := makeCommit(r, tree, parents, body)
	setRefTo(r, "refs/heads/"+p.Base, sha)
	p.Merged = true
	t := nextISO()
	p.MergedAt = &t
	s := sha
	p.MergeSha = &s
	p.HeadSha = head
	p.BaseSha = base
	setIssueState(p.Issue, "closed")
	t2 := t
	p.Issue.Closed = &t2
	return sha, 0, ""
}
