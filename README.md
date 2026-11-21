# makefun-github-clone

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ryanp1632-netizen/makefun-github-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ryanp1632-netizen/makefun-github-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge with zero dependencies: git smart HTTP (protocol v2
upload-pack, v0 receive-pack), the REST v3 API, GraphQL v4 and an MCP endpoint, all over one in-memory
store. It is written twice: a native Go server (`native/`, prebuilt as `bin/forge-linux-amd64` because
the grader image only has Node 22) that `main.ts` starts, and the original Node implementation (`src/`)
that serves whenever the binary is missing or cannot start.

## What it serves

| Surface | Path | Notes |
| --- | --- | --- |
| Git smart HTTP | `/{owner}/{repo}.git` | `info/refs`, `git-upload-pack` (v2 `ls-refs`/`fetch` with shallow and deepen, plus v0), `git-receive-pack` (report-status, side-band-64k, delete-refs, fast-forward only) |
| REST v3 | `/api/v3` | repos, contents, git refs, branches, commits, trees, blobs, labels, issues, comments, pull requests, files, squash/merge |
| GraphQL v4 | `/api/graphql` | lexer, parser and executor over an SDL schema; introspection, fragments, aliases, batching, cursor connections, mutations |
| MCP | `/mcp` | JSON-RPC 2.0 over streamable HTTP; `server/discover` (2026-07-28) and the `initialize` handshake; 18 tools |

Packs are written by hand (`node:zlib` + `node:crypto`); packs pushed to it may hold OFS/REF deltas
and thin bases. Pull request merges are a three-way tree merge with conflict detection.

## Layout

- `main.ts` — the trial lifecycle: start the native server (or the Node fallback), register, wait for done.
- `native/*.go` — the native server: a raw HTTP/1.1 loop, git objects and packs, smart HTTP, store, REST,
  GraphQL and MCP, mirroring `src/` file for file. `bin/forge-linux-amd64` is its static build.
- `src/app.ts` — HTTP routing, auth and reply headers.
- `src/gitobj.ts` — objects, trees, commits, pkt-lines, pack read/write.
- `src/githttp.ts` — the smart HTTP protocol.
- `src/store.ts` — repositories, refs, issues, pulls, merges.
- `src/rest.ts`, `src/graphql.ts`, `src/mcp.ts` — the three APIs.

## Run it

```sh
MAKEFUN_SHOP_TOKEN=dev FORGE_PORT=8000 node main.ts
```

Without `MAKEFUN_ENV_URL` it serves until killed; requests need `Authorization: Bearer dev`.
`FORGE_NATIVE=0` forces the Node implementation. Rebuild the binary with Go 1.24+:

```sh
cd native && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o ../bin/forge-linux-amd64 .
```

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
