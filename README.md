# makefun-github-clone

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ryanp1632-netizen/makefun-github-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge in one Node 22 process with zero dependencies: git smart
HTTP (protocol v2 upload-pack, v0 receive-pack), the REST v3 API, GraphQL v4 and an MCP endpoint, all
over one in-memory store.

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

- `main.ts` — the trial lifecycle (bind an ephemeral port, register, wait for done).
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

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
