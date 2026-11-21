// Git objects, trees, commits, pkt-lines and packfiles: everything below the protocol, in memory.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ZERO = "0000000000000000000000000000000000000000"

// Obj is one git object. Type is "commit", "tree", "blob" or "tag".
type Obj struct {
	Type   string
	Data   []byte
	z      []byte
	commit *CommitInfo
}

var typeNum = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var numType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	var sum [20]byte
	return hex.EncodeToString(h.Sum(sum[:0]))
}

func isOid(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------ trees

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			return nil, errors.New("bad tree")
		}
		sp += i
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 {
			return nil, errors.New("bad tree")
		}
		nul += sp
		if nul+21 > len(data) {
			return nil, errors.New("bad tree")
		}
		out = append(out, TreeEntry{Mode: string(data[i:sp]), Name: string(data[sp+1 : nul]), Oid: hex.EncodeToString(data[nul+1 : nul+21])})
		i = nul + 21
	}
	return out, nil
}

func mustTree(data []byte) []TreeEntry {
	t, err := parseTree(data)
	if err != nil {
		panic(err)
	}
	return t
}

func sortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(a, b int) bool { return sortKey(sorted[a]) < sortKey(sorted[b]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ------------------------------------------------------------------ commits and tags

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) *CommitInfo {
	text := toValidUTF8(data)
	head, message := text, ""
	if k := strings.Index(text, "\n\n"); k >= 0 {
		head, message = text[:k], text[k+2:]
	}
	info := &CommitInfo{Message: message}
	for _, line := range strings.Split(head, "\n") {
		sp := strings.IndexByte(line, ' ')
		var key, val string
		if sp < 0 {
			key, val = line[:max(0, len(line)-1)], line
		} else {
			key, val = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			info.Tree = val
		case "parent":
			info.Parents = append(info.Parents, val)
		case "author":
			info.Author = val
		case "committer":
			info.Committer = val
		}
	}
	return info
}

func toValidUTF8(b []byte) string { return strings.ToValidUTF8(string(b), "�") }

var tagObjRe = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})$`)
var tagTypeRe = regexp.MustCompile(`(?m)^type (\w+)$`)

func parseTagTarget(data []byte) (object, typ string) {
	n := len(data)
	if n > 400 {
		n = 400
	}
	text := toValidUTF8(data[:n])
	if m := tagObjRe.FindStringSubmatch(text); m != nil {
		object = m[1]
	}
	if m := tagTypeRe.FindStringSubmatch(text); m != nil {
		typ = m[1]
	}
	return
}

var identRe = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d{4})$`)

func isoOfSec(sec int64) string { return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z") }

// parseIdent splits "Name <email> 1735689600 +0000" into a {name, email, date} object.
func parseIdent(ident string) (name, email, date string) {
	m := identRe.FindStringSubmatch(ident)
	if m == nil {
		return ident, "", "1970-01-01T00:00:00.000Z"
	}
	sec, _ := strconv.ParseInt(m[3], 10, 64)
	return m[1], m[2], isoOfSec(sec)
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + author + "\ncommitter " + committer + "\n\n" + message)
	return []byte(sb.String())
}

// ------------------------------------------------------------------ pkt-lines

var FLUSH = []byte("0000")
var DELIM = []byte("0001")

func pkt(payload string) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(payload)+4)), payload...)
}

func appendPkt(b []byte, payload string) []byte {
	b = append(b, fmt.Sprintf("%04x", len(payload)+4)...)
	return append(b, payload...)
}

type Packet struct {
	Kind    byte // 'd' data, 'f' flush, 'l' delim, 'e' end
	Payload []byte
}

func readPackets(buf []byte, pos int, untilFlush bool) ([]Packet, int, error) {
	var packets []Packet
	i := pos
	for i+4 <= len(buf) {
		n, err := strconv.ParseUint(string(buf[i:i+4]), 16, 16)
		if err != nil {
			return nil, 0, fmt.Errorf("bad pkt-line length %s", jsonStringifyValue(string(buf[i:i+4]), true))
		}
		switch n {
		case 0:
			packets = append(packets, Packet{Kind: 'f'})
			i += 4
			if untilFlush {
				return packets, i, nil
			}
			continue
		case 1:
			packets = append(packets, Packet{Kind: 'l'})
			i += 4
			continue
		case 2:
			packets = append(packets, Packet{Kind: 'e'})
			i += 4
			continue
		}
		if n < 4 || i+int(n) > len(buf) {
			return nil, 0, errors.New("truncated pkt-line")
		}
		packets = append(packets, Packet{Kind: 'd', Payload: buf[i+4 : i+int(n)]})
		i += int(n)
	}
	if i != len(buf) && untilFlush {
		return nil, 0, errors.New("truncated pkt-line stream")
	}
	return packets, i, nil
}

// appendSideband frames data as side-band-64k packets on the given band.
func appendSideband(out []byte, band byte, data []byte) []byte {
	const maxChunk = 65515
	for i := 0; i < len(data); i += maxChunk {
		end := i + maxChunk
		if end > len(data) {
			end = len(data)
		}
		out = append(out, fmt.Sprintf("%04x", end-i+5)...)
		out = append(out, band)
		out = append(out, data[i:end]...)
	}
	return out
}

// ------------------------------------------------------------------ packfiles

func zlibOf(o *Obj) []byte {
	if o.z == nil {
		var buf bytes.Buffer
		w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		w.Write(o.Data)
		w.Close()
		o.z = buf.Bytes()
	}
	return o.z
}

func writePack(objects []*Obj) []byte {
	size := 32
	for _, o := range objects {
		size += len(zlibOf(o)) + 10
	}
	out := make([]byte, 12, size)
	copy(out, "PACK")
	binary.BigEndian.PutUint32(out[4:], 2)
	binary.BigEndian.PutUint32(out[8:], uint32(len(objects)))
	for _, o := range objects {
		n := uint64(len(o.Data))
		c := byte(typeNum[o.Type]<<4) | byte(n&0x0f)
		n >>= 4
		for n > 0 {
			out = append(out, c|0x80)
			c = byte(n & 0x7f)
			n >>= 7
		}
		out = append(out, c)
		out = append(out, zlibOf(o)...)
	}
	sum := sha1.Sum(out)
	return append(out, sum[:]...)
}

func applyDelta(base, delta []byte) ([]byte, error) {
	i := 0
	varint := func() (int, error) {
		result, shift := 0, uint(0)
		for {
			if i >= len(delta) {
				return 0, errors.New("bad delta")
			}
			b := delta[i]
			i++
			result |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return result, nil
			}
		}
	}
	baseSize, err := varint()
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	resultSize, err := varint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, resultSize)
	for i < len(delta) {
		op := delta[i]
		i++
		if op&0x80 != 0 {
			off, ln := 0, 0
			for bit, sh := 0, 0; bit < 4; bit, sh = bit+1, sh+8 {
				if op&(1<<bit) != 0 {
					if i >= len(delta) {
						return nil, errors.New("bad delta")
					}
					off |= int(delta[i]) << sh
					i++
				}
			}
			for bit, sh := 4, 0; bit < 7; bit, sh = bit+1, sh+8 {
				if op&(1<<bit) != 0 {
					if i >= len(delta) {
						return nil, errors.New("bad delta")
					}
					ln |= int(delta[i]) << sh
					i++
				}
			}
			if ln == 0 {
				ln = 0x10000
			}
			if off < 0 || off+ln > len(base) {
				return nil, errors.New("bad delta copy")
			}
			out = append(out, base[off:off+ln]...)
		} else if op > 0 {
			if i+int(op) > len(delta) {
				return nil, errors.New("bad delta")
			}
			out = append(out, delta[i:i+int(op)]...)
			i += int(op)
		} else {
			return nil, errors.New("bad delta opcode")
		}
	}
	if len(out) != resultSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

var zreader io.ReadCloser

func inflateAt(buf []byte, pos int, expected int) ([]byte, int, error) {
	if pos > len(buf) {
		return nil, 0, errors.New("unexpected end of file")
	}
	src := bytes.NewReader(buf[pos:])
	var err error
	if zreader == nil {
		zreader, err = zlib.NewReader(src)
	} else {
		err = zreader.(zlib.Resetter).Reset(src, nil)
	}
	if err != nil {
		zreader = nil
		return nil, 0, err
	}
	out := make([]byte, expected, expected+1)
	if _, err := io.ReadFull(zreader, out); err != nil {
		return nil, 0, errors.New("inflated size mismatch")
	}
	var one [1]byte
	if n, err := zreader.Read(one[:]); n != 0 || err != io.EOF {
		if n != 0 {
			return nil, 0, errors.New("inflated size mismatch")
		}
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
	}
	return out, pos + (len(buf) - pos - src.Len()), nil
}

type pendingDelta struct {
	offset     int
	baseOffset int
	baseOid    string
	delta      []byte
}

// readPack parses a pack into objects keyed by oid; lookup resolves thin-pack REF_DELTA bases.
func readPack(pack []byte, lookup func(string) *Obj) (map[string]*Obj, error) {
	out := map[string]*Obj{}
	if len(pack) == 0 {
		return out, nil
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:]))
	end := len(pack) - 20
	sum := sha1.Sum(pack[:end])
	if !bytes.Equal(sum[:], pack[end:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	byOffset := map[int]*Obj{}
	var pending []pendingDelta
	pos := 12
	for n := 0; n < count; n++ {
		start := pos
		if pos >= end {
			return nil, errors.New("pack entries overrun the trailer")
		}
		c := pack[pos]
		pos++
		tn := (c >> 4) & 7
		size := int(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= end {
				return nil, errors.New("pack entries overrun the trailer")
			}
			c = pack[pos]
			pos++
			size |= int(c&0x7f) << shift
			shift += 7
		}
		switch tn {
		case 6:
			c = pack[pos]
			pos++
			off := int(c & 0x7f)
			for c&0x80 != 0 {
				c = pack[pos]
				pos++
				off = (off+1)*128 + int(c&0x7f)
			}
			data, next, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos = next
			pending = append(pending, pendingDelta{offset: start, baseOffset: start - off, delta: data})
		case 7:
			if pos+20 > end {
				return nil, errors.New("pack entries overrun the trailer")
			}
			baseOid := hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			data, next, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos = next
			pending = append(pending, pendingDelta{offset: start, baseOffset: -1, baseOid: baseOid, delta: data})
		default:
			typ, ok := numType[tn]
			if !ok {
				return nil, fmt.Errorf("bad pack object type %d", tn)
			}
			data, next, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos = next
			o := &Obj{Type: typ, Data: data}
			byOffset[start] = o
			out[hashObject(typ, data)] = o
		}
		if pos > end {
			return nil, errors.New("pack entries overrun the trailer")
		}
	}
	progress := true
	for len(pending) > 0 && progress {
		progress = false
		for k := len(pending) - 1; k >= 0; k-- {
			p := pending[k]
			var base *Obj
			if p.baseOffset >= 0 {
				base = byOffset[p.baseOffset]
			} else {
				base = out[p.baseOid]
				if base == nil {
					base = lookup(p.baseOid)
				}
			}
			if base == nil {
				continue
			}
			data, err := applyDelta(base.Data, p.delta)
			if err != nil {
				return nil, err
			}
			o := &Obj{Type: base.Type, Data: data}
			byOffset[p.offset] = o
			out[hashObject(o.Type, data)] = o
			pending = append(pending[:k], pending[k+1:]...)
			progress = true
		}
	}
	if len(pending) > 0 {
		return nil, errors.New("unresolved delta base")
	}
	return out, nil
}
