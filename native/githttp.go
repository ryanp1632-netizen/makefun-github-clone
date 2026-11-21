// Git smart HTTP: info/refs advertisements, upload-pack (protocol v2 and v0) and receive-pack (v0).

package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "agent=forge/1.0"

func gitSend(status int, typ string, body []byte) *Resp {
	return &Resp{Status: status, Headers: []KV{{"content-type", typ}, {"content-length", strconv.Itoa(len(body))}, {"cache-control", "no-cache"}}, Body: body}
}

func gitNotFound() *Resp {
	return gitSend(404, jsonType, []byte("{\"message\":\"Repository not found\"}\n"))
}

func headTarget(r *Repo) string { return "refs/heads/" + r.DefaultBranch }

func sortedRefs(r *Repo) [][2]string {
	e := refEntries(r)
	sort.SliceStable(e, func(a, b int) bool { return e[a][0] < e[b][0] })
	return e
}

func peeled(r *Repo, oid string) (string, bool) {
	o := r.Objects[oid]
	if o == nil || o.Type != "tag" {
		return "", false
	}
	if c, ok := peelToCommit(r, oid); ok {
		return c, true
	}
	t, _ := parseTagTarget(o.Data)
	return t, true
}

func v0Advertisement(r *Repo, service, caps string) []byte {
	out := pkt("# service=" + service + "\n")
	out = append(out, FLUSH...)
	var lines []string
	if head, ok := r.Refs[headTarget(r)]; ok && service == "git-upload-pack" {
		lines = append(lines, head+" HEAD")
	}
	for _, e := range sortedRefs(r) {
		lines = append(lines, e[1]+" "+e[0])
		if service == "git-upload-pack" {
			if p, ok := peeled(r, e[1]); ok {
				lines = append(lines, p+" "+e[0]+"^{}")
			}
		}
	}
	if len(lines) == 0 {
		lines = append(lines, ZERO+" capabilities^{}")
	}
	for i, l := range lines {
		if i == 0 {
			out = appendPkt(out, l+"\x00"+caps+"\n")
		} else {
			out = appendPkt(out, l+"\n")
		}
	}
	return append(out, FLUSH...)
}

var v2Advert = func() []byte {
	var b []byte
	for _, l := range []string{"version 2\n", gitAgent + "\n", "ls-refs=unborn\n", "fetch=shallow\n", "server-option\n", "object-format=sha1\n"} {
		b = appendPkt(b, l)
	}
	return append(b, FLUSH...)
}()

var version2Re = regexp.MustCompile(`version=2`)

func handleInfoRefs(req *Req, owner, name, service string) *Resp {
	r := getRepo(owner, name)
	if r == nil {
		return gitNotFound()
	}
	if service == "git-upload-pack" {
		if version2Re.MatchString(req.Headers["git-protocol"]) {
			return gitSend(200, "application/x-git-upload-pack-advertisement", v2Advert)
		}
		caps := "multi_ack_detailed side-band-64k ofs-delta shallow no-progress thin-pack include-tag symref=HEAD:" + headTarget(r) + " object-format=sha1 " + gitAgent
		return gitSend(200, "application/x-git-upload-pack-advertisement", cachedResponse(r, "adv-upload\n"+caps, func() []byte { return v0Advertisement(r, service, caps) }))
	}
	if service == "git-receive-pack" {
		caps := "report-status side-band-64k delete-refs ofs-delta quiet object-format=sha1 " + gitAgent
		return gitSend(200, "application/x-git-receive-pack-advertisement", cachedResponse(r, "adv-receive", func() []byte { return v0Advertisement(r, service, caps) }))
	}
	return gitSend(403, jsonType, []byte("{\"message\":\"Unsupported service\"}\n"))
}

// ------------------------------------------------------------------ object selection

type oidSet map[string]struct{}

func closure(r *Repo, roots []string, cut oidSet, commitLimit map[string]int, out *[]string) oidSet {
	seen := oidSet{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		o := r.Objects[oid]
		if o == nil {
			continue
		}
		seen[oid] = struct{}{}
		if out != nil {
			*out = append(*out, oid)
		}
		switch o.Type {
		case "commit":
			c := commitOf(r, oid)
			if c == nil {
				continue
			}
			stack = append(stack, c.Tree)
			if _, ok := cut[oid]; ok {
				continue
			}
			if commitLimit != nil {
				if _, ok := commitLimit[oid]; !ok {
					continue
				}
			}
			for _, p := range c.Parents {
				if commitLimit == nil {
					stack = append(stack, p)
				} else if _, ok := commitLimit[p]; ok {
					stack = append(stack, p)
				}
			}
		case "tree":
			for _, e := range mustTree(o.Data) {
				if e.Mode != "160000" {
					stack = append(stack, e.Oid)
				}
			}
		case "tag":
			t, _ := parseTagTarget(o.Data)
			stack = append(stack, t)
		}
	}
	return seen
}

type fetchArgs struct {
	wants, haves, shallow []string
	depth                 int // 0 = none
	done                  bool
}

type fetchPlan struct {
	objects   []*Obj
	shallow   []string
	unshallow []string
}

type fetchResult struct {
	pack []byte
	plan *fetchPlan
}

func planFetch(r *Repo, a *fetchArgs) *fetchPlan {
	clientShallow := oidSet{}
	for _, o := range a.shallow {
		if r.Objects[o] != nil {
			clientShallow[o] = struct{}{}
		}
	}
	var limit map[string]int
	var limitOrder []string
	shallowOut := []string{}
	unshallow := []string{}
	if a.depth > 0 {
		limit = map[string]int{}
		var frontier []string
		for _, w := range a.wants {
			if c, ok := peelToCommit(r, w); ok {
				if _, seen := limit[c]; !seen {
					limit[c] = 1
					limitOrder = append(limitOrder, c)
					frontier = append(frontier, c)
				}
			}
		}
		for d := 1; d < a.depth; d++ {
			var next []string
			for _, oid := range frontier {
				c := commitOf(r, oid)
				if c == nil {
					continue
				}
				for _, p := range c.Parents {
					if _, seen := limit[p]; !seen && r.Objects[p] != nil {
						limit[p] = d + 1
						limitOrder = append(limitOrder, p)
						next = append(next, p)
					}
				}
			}
			frontier = next
		}
		for _, oid := range limitOrder {
			d := limit[oid]
			c := commitOf(r, oid)
			if c == nil {
				continue
			}
			hasParents := false
			for _, p := range c.Parents {
				if r.Objects[p] != nil {
					hasParents = true
					break
				}
			}
			_, isClientShallow := clientShallow[oid]
			if d == a.depth && hasParents {
				if !isClientShallow {
					shallowOut = append(shallowOut, oid)
				}
			} else if isClientShallow && hasParents {
				unshallow = append(unshallow, oid)
			}
		}
	}
	var haves []string
	for _, h := range a.haves {
		if r.Objects[h] != nil {
			haves = append(haves, h)
		}
	}
	have := oidSet{}
	if len(haves) > 0 {
		have = closure(r, haves, clientShallow, nil, nil)
	}
	var wantOrder []string
	closure(r, a.wants, oidSet{}, limit, &wantOrder)
	order := map[string]int{"commit": 0, "tag": 1, "tree": 2, "blob": 3}
	var picked []*Obj
	for _, oid := range wantOrder {
		if _, ok := have[oid]; ok {
			continue
		}
		if o := r.Objects[oid]; o != nil {
			picked = append(picked, o)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return order[picked[i].Type] < order[picked[j].Type] })
	sort.Strings(shallowOut)
	sort.Strings(unshallow)
	return &fetchPlan{objects: picked, shallow: shallowOut, unshallow: unshallow}
}

func fetchKey(a *fetchArgs) string {
	d := ""
	if a.depth > 0 {
		d = strconv.Itoa(a.depth)
	}
	return strings.Join(a.wants, ",") + "|" + strings.Join(a.haves, ",") + "|" + strings.Join(a.shallow, ",") + "|" + d
}

func fetchPack(r *Repo, a *fetchArgs) *fetchResult {
	k := fetchKey(a)
	if r.planCache == nil || r.planVersion != r.RefVersion {
		r.planCache = map[string]*fetchResult{}
		r.planVersion = r.RefVersion
	}
	if hit := r.planCache[k]; hit != nil {
		return hit
	}
	plan := planFetch(r, a)
	pack, ok := r.packCache[k]
	if !ok {
		pack = writePack(plan.objects)
		if len(r.packCache) > 64 {
			r.packCache = map[string][]byte{}
		}
		r.packCache[k] = pack
	}
	res := &fetchResult{pack: pack, plan: plan}
	if len(r.planCache) > 64 {
		r.planCache = map[string]*fetchResult{}
	}
	r.planCache[k] = res
	return res
}

// ------------------------------------------------------------------ upload-pack

func dataLines(packets []Packet) []string {
	var out []string
	for _, p := range packets {
		if p.Kind == 'd' {
			out = append(out, strings.TrimSuffix(toValidUTF8(p.Payload), "\n"))
		}
	}
	return out
}

func lsRefs(r *Repo, args []string) []byte {
	has := func(x string) bool {
		for _, a := range args {
			if a == x {
				return true
			}
		}
		return false
	}
	peel, symrefs, unborn := has("peel"), has("symrefs"), has("unborn")
	var prefixes []string
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[11:])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	target := headTarget(r)
	sym := ""
	if symrefs {
		sym = " symref-target:" + target
	}
	if keep("HEAD") {
		if head, ok := r.Refs[target]; ok {
			out = appendPkt(out, head+" HEAD"+sym+"\n")
		} else if unborn {
			out = appendPkt(out, "unborn HEAD"+sym+"\n")
		}
	}
	for _, e := range sortedRefs(r) {
		if !keep(e[0]) {
			continue
		}
		line := e[1] + " " + e[0]
		if peel {
			if p, ok := peeled(r, e[1]); ok {
				line += " peeled:" + p
			}
		}
		out = appendPkt(out, line+"\n")
	}
	return append(out, FLUSH...)
}

func parseFetchArgs(args []string) *fetchArgs {
	out := &fetchArgs{}
	seen := map[string]bool{}
	for _, a := range args {
		sp := strings.IndexByte(a, ' ')
		word, val := a, ""
		if sp >= 0 {
			word = a[:sp]
			val = a[sp+1:]
			if k := strings.IndexByte(val, ' '); k >= 0 {
				val = val[:k]
			}
		}
		switch {
		case word == "want" && isOid(val):
			if !seen[val] {
				seen[val] = true
				out.wants = append(out.wants, val)
			}
		case word == "have" && isOid(val):
			out.haves = append(out.haves, val)
		case word == "shallow" && isOid(val):
			out.shallow = append(out.shallow, val)
		case word == "deepen":
			f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			if err == nil && f != 0 {
				out.depth = int(f)
			} else {
				out.depth = 0
			}
		case word == "done":
			out.done = true
		}
	}
	return out
}

func cachedResponse(r *Repo, key string, build func() []byte) []byte {
	if r.respCache == nil || r.respVersion != r.RefVersion {
		r.respCache = map[string][]byte{}
		r.respVersion = r.RefVersion
	}
	hit, ok := r.respCache[key]
	if !ok {
		hit = build()
		if len(r.respCache) > 128 {
			r.respCache = map[string][]byte{}
		}
		r.respCache[key] = hit
	}
	return hit
}

func fetchV2Build(r *Repo, args []string) []byte {
	fa := parseFetchArgs(args)
	var out []byte
	if !fa.done && len(fa.haves) > 0 {
		out = appendPkt(out, "acknowledgments\n")
		var common []string
		for _, h := range fa.haves {
			if r.Objects[h] != nil {
				common = append(common, h)
			}
		}
		if len(common) == 0 {
			out = appendPkt(out, "NAK\n")
		}
		for _, h := range common {
			out = appendPkt(out, "ACK "+h+"\n")
		}
		out = appendPkt(out, "ready\n")
		out = append(out, DELIM...)
	}
	res := fetchPack(r, fa)
	if fa.depth != 0 || len(fa.shallow) > 0 {
		out = appendPkt(out, "shallow-info\n")
		for _, s := range res.plan.shallow {
			out = appendPkt(out, "shallow "+s+"\n")
		}
		for _, u := range res.plan.unshallow {
			out = appendPkt(out, "unshallow "+u+"\n")
		}
		out = append(out, DELIM...)
	}
	out = appendPkt(out, "packfile\n")
	out = appendSideband(out, 1, res.pack)
	return append(out, FLUSH...)
}

var multiAckRe = regexp.MustCompile(`multi_ack`)

func uploadPackV0(r *Repo, body []byte) ([]byte, error) {
	first, next, err := readPackets(body, 0, true)
	if err != nil {
		return nil, err
	}
	wantLines := dataLines(first)
	var haveLines []string
	if next < len(body) {
		second, _, err := readPackets(body, next, false)
		if err != nil {
			return nil, err
		}
		haveLines = dataLines(second)
	}
	fa := parseFetchArgs(append(append([]string(nil), wantLines...), haveLines...))
	if len(fa.wants) == 0 {
		return FLUSH, nil
	}
	res := fetchPack(r, fa)
	var out []byte
	if fa.depth != 0 {
		for _, s := range res.plan.shallow {
			out = appendPkt(out, "shallow "+s+"\n")
		}
		for _, u := range res.plan.unshallow {
			out = appendPkt(out, "unshallow "+u+"\n")
		}
		out = append(out, FLUSH...)
	}
	var common []string
	for _, h := range fa.haves {
		if r.Objects[h] != nil {
			common = append(common, h)
		}
	}
	firstLine := ""
	if len(wantLines) > 0 {
		firstLine = wantLines[0]
	}
	multiAck := multiAckRe.MatchString(firstLine)
	if len(common) > 0 && multiAck {
		for _, h := range common {
			out = appendPkt(out, "ACK "+h+" common\n")
		}
		out = appendPkt(out, "ACK "+common[len(common)-1]+"\n")
	} else if len(common) > 0 {
		out = appendPkt(out, "ACK "+common[0]+"\n")
	} else {
		out = appendPkt(out, "NAK\n")
	}
	out = appendSideband(out, 1, res.pack)
	return append(out, FLUSH...), nil
}

func handleUploadPack(req *Req, owner, name string, body []byte) (resp *Resp) {
	r := getRepo(owner, name)
	if r == nil {
		return gitNotFound()
	}
	const typ = "application/x-git-upload-pack-result"
	defer func() {
		if p := recover(); p != nil {
			msg := panicMessage(p)
			resp = gitSend(400, jsonType, append(toJSON(O{{"message", "bad upload-pack request: " + msg}}), '\n'))
		}
	}()
	v2 := version2Re.MatchString(req.Headers["git-protocol"]) || (len(body) >= 12 && string(body[4:12]) == "command=")
	if !v2 {
		out, err := uploadPackV0(r, body)
		if err != nil {
			return gitSend(400, jsonType, append(toJSON(O{{"message", "bad upload-pack request: " + err.Error()}}), '\n'))
		}
		return gitSend(200, typ, out)
	}
	packets, _, err := readPackets(body, 0, false)
	if err != nil {
		return gitSend(400, jsonType, append(toJSON(O{{"message", "bad upload-pack request: " + err.Error()}}), '\n'))
	}
	command := ""
	var args []string
	inArgs := false
	for _, p := range packets {
		if p.Kind == 'l' {
			inArgs = true
			continue
		}
		if p.Kind == 'f' {
			break
		}
		if p.Kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(toValidUTF8(p.Payload), "\n")
		if !inArgs {
			if strings.HasPrefix(line, "command=") {
				command = line[8:]
			}
		} else {
			args = append(args, line)
		}
	}
	switch command {
	case "ls-refs":
		return gitSend(200, typ, cachedResponse(r, "ls-refs\n"+strings.Join(args, "\n"), func() []byte { return lsRefs(r, args) }))
	case "fetch":
		return gitSend(200, typ, cachedResponse(r, "fetch\n"+strings.Join(args, "\n"), func() []byte { return fetchV2Build(r, args) }))
	}
	q := string(toJSON(command))
	return gitSend(400, jsonType, []byte("{\"message\":\"unknown command "+q[1:len(q)-1]+"\"}\n"))
}

func panicMessage(p any) string {
	switch x := p.(type) {
	case error:
		return x.Error()
	case string:
		return x
	}
	return "error"
}

// ------------------------------------------------------------------ receive-pack

type refCommand struct{ old, neu, ref string }

func handleReceivePack(owner, name string, body []byte) *Resp {
	r := getRepo(owner, name)
	if r == nil {
		return gitNotFound()
	}
	var commands []refCommand
	caps := map[string]bool{}
	packets, packStart, err := readPackets(body, 0, true)
	if err == nil {
		for _, p := range packets {
			if p.Kind != 'd' {
				continue
			}
			line := strings.TrimSuffix(toValidUTF8(p.Payload), "\n")
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				caps = map[string]bool{}
				for _, c := range strings.Split(line[nul+1:], " ") {
					caps[c] = true
				}
				line = line[:nul]
			}
			f := strings.Split(line, " ")
			if len(f) < 3 || !isOid(f[0]) || !isOid(f[1]) || !strings.HasPrefix(f[2], "refs/") {
				err = errBadCommand
				break
			}
			commands = append(commands, refCommand{f[0], f[1], f[2]})
		}
	}
	if err != nil {
		return gitSend(400, jsonType, append(toJSON(O{{"message", "bad receive-pack request: " + err.Error()}}), '\n'))
	}
	unpack := "ok"
	if pack := body[packStart:]; len(pack) > 0 {
		objs, perr := readPack(pack, func(oid string) *Obj { return r.Objects[oid] })
		if perr != nil {
			unpack = strings.ReplaceAll(perr.Error(), "\n", " ")
		} else {
			for oid, o := range objs {
				if r.Objects[oid] == nil {
					r.Objects[oid] = o
				}
			}
		}
	}
	var inner []byte
	inner = appendPkt(inner, "unpack "+unpack+"\n")
	for _, c := range commands {
		current := refOr(r, c.ref, ZERO)
		ng := ""
		switch {
		case unpack != "ok":
			ng = "unpacker error"
		case c.neu != ZERO && r.Objects[c.neu] == nil:
			ng = "missing necessary objects"
		case current != c.old:
			if c.old == ZERO {
				ng = "already exists"
			} else if current == ZERO {
				ng = "no such ref"
			} else {
				ng = "fetch first"
			}
		case c.neu != ZERO && c.old != ZERO:
			oldC, ok1 := peelToCommit(r, c.old)
			newC, ok2 := peelToCommit(r, c.neu)
			ff := !strings.HasPrefix(c.ref, "refs/tags/") && ok1 && ok2 && isAncestor(r, oldC, newC)
			if !ff {
				ng = "non-fast-forward"
			}
		}
		if ng != "" {
			inner = appendPkt(inner, "ng "+c.ref+" "+ng+"\n")
		} else {
			if c.neu == ZERO {
				setRef(r, c.ref, nil)
			} else {
				setRefTo(r, c.ref, c.neu)
			}
			inner = appendPkt(inner, "ok "+c.ref+"\n")
		}
	}
	inner = append(inner, FLUSH...)
	const typ = "application/x-git-receive-pack-result"
	if caps["side-band-64k"] || caps["side-band"] {
		out := appendSideband(nil, 1, inner)
		return gitSend(200, typ, append(out, FLUSH...))
	}
	if !caps["report-status"] && !caps["report-status-v2"] {
		return gitSend(200, typ, []byte{})
	}
	return gitSend(200, typ, inner)
}

type strErr string

func (e strErr) Error() string { return string(e) }

var errBadCommand = strErr("bad command")
