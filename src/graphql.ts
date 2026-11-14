// GitHub GraphQL v4 at /api/graphql: a small spec-shaped engine (lexer, parser, executor and
// introspection) over a schema written in SDL below, with resolvers on the shared store.

import { VIEWER } from './identity.ts';
import * as S from './store.ts';
import type { Issue, Pull, Repo } from './store.ts';
import { HttpError, createIssueChecked, createPullChecked, createRepoChecked } from './rest.ts';

// ------------------------------------------------------------------ lexer

type Tok = { k: 'name' | 'punct' | 'string' | 'int' | 'float' | 'eof'; v: string };

function lex(src: string): Tok[] {
  const out: Tok[] = [];
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',' || c === '﻿') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < n && src[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && src.startsWith('...', i)) {
      out.push({ k: 'punct', v: '...' });
      i += 3;
      continue;
    }
    if ('!$&()[]{}:=@|'.includes(c)) {
      out.push({ k: 'punct', v: c });
      i += 1;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i + 1;
      while (j < n && /[A-Za-z0-9_]/.test(src[j])) j += 1;
      out.push({ k: 'name', v: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[-0-9]/.test(c)) {
      const m = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(src.slice(i));
      if (m === null) throw new Error(`Unexpected character ${JSON.stringify(c)}`);
      out.push({ k: m[2] || m[3] ? 'float' : 'int', v: m[0] });
      i += m[0].length;
      continue;
    }
    if (c === '"') {
      if (src.startsWith('"""', i)) {
        const end = src.indexOf('"""', i + 3);
        if (end < 0) throw new Error('Unterminated block string');
        const raw = src.slice(i + 3, end);
        const lines = raw.split(/\r?\n/);
        let indent = Infinity;
        for (const line of lines.slice(1)) {
          const m = /^[ \t]*/.exec(line) as RegExpExecArray;
          if (m[0].length < line.length) indent = Math.min(indent, m[0].length);
        }
        const trimmed = lines.map((l, k) => (k === 0 || indent === Infinity ? l : l.slice(indent)));
        while (trimmed.length > 0 && trimmed[0].trim() === '') trimmed.shift();
        while (trimmed.length > 0 && trimmed[trimmed.length - 1].trim() === '') trimmed.pop();
        out.push({ k: 'string', v: trimmed.join('\n') });
        i = end + 3;
        continue;
      }
      let j = i + 1;
      let s = '';
      while (j < n && src[j] !== '"') {
        if (src[j] === '\\') {
          const e = src[j + 1];
          if (e === 'u') {
            s += String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16));
            j += 6;
            continue;
          }
          s += ({ n: '\n', t: '\t', r: '\r', b: '\b', f: '\f', '"': '"', '\\': '\\', '/': '/' } as Record<string, string>)[e] ?? e;
          j += 2;
          continue;
        }
        if (src[j] === '\n') throw new Error('Unterminated string');
        s += src[j];
        j += 1;
      }
      if (j >= n) throw new Error('Unterminated string');
      out.push({ k: 'string', v: s });
      i = j + 1;
      continue;
    }
    throw new Error(`Unexpected character ${JSON.stringify(c)}`);
  }
  out.push({ k: 'eof', v: '' });
  return out;
}

// ------------------------------------------------------------------ AST

type Value =
  | { kind: 'var'; name: string }
  | { kind: 'int' | 'float' | 'string' | 'boolean' | 'null' | 'enum'; value: unknown }
  | { kind: 'list'; items: Value[] }
  | { kind: 'object'; fields: Array<[string, Value]> };

interface Field { kind: 'field'; alias: string | null; name: string; args: Array<[string, Value]>; sel: Selection[] | null }
interface Inline { kind: 'inline'; on: string | null; sel: Selection[] }
interface Spread { kind: 'spread'; name: string }
type Selection = Field | Inline | Spread;

interface Operation { op: 'query' | 'mutation' | 'subscription'; name: string | null; vars: Array<{ name: string; def: Value | null }>; sel: Selection[] }
interface Doc { ops: Operation[]; frags: Map<string, { on: string; sel: Selection[] }> }

class Parser {
  t: Tok[];
  p = 0;
  constructor(src: string) {
    this.t = lex(src);
  }
  peek(v?: string): boolean {
    const tok = this.t[this.p];
    return v === undefined ? true : (tok.k === 'punct' || tok.k === 'name') && tok.v === v;
  }
  next(): Tok {
    return this.t[this.p++];
  }
  expect(v: string): void {
    const tok = this.next();
    if (tok.v !== v || (tok.k !== 'punct' && tok.k !== 'name')) throw new Error(`Expected ${JSON.stringify(v)}, found ${tok.k === 'eof' ? '<EOF>' : JSON.stringify(tok.v)}`);
  }
  name(): string {
    const tok = this.next();
    if (tok.k !== 'name') throw new Error(`Expected a name, found ${tok.k === 'eof' ? '<EOF>' : JSON.stringify(tok.v)}`);
    return tok.v;
  }
  doc(): Doc {
    const doc: Doc = { ops: [], frags: new Map() };
    while (this.t[this.p].k !== 'eof') {
      if (this.peek('{')) doc.ops.push({ op: 'query', name: null, vars: [], sel: this.selSet() });
      else if (this.peek('fragment')) {
        this.next();
        const name = this.name();
        this.expect('on');
        const on = this.name();
        this.directives();
        doc.frags.set(name, { on, sel: this.selSet() });
      } else {
        const opTok = this.name();
        if (opTok !== 'query' && opTok !== 'mutation' && opTok !== 'subscription') throw new Error(`Unexpected ${JSON.stringify(opTok)}`);
        let name: string | null = null;
        if (this.t[this.p].k === 'name') name = this.name();
        const vars: Operation['vars'] = [];
        if (this.peek('(')) {
          this.next();
          while (!this.peek(')')) {
            this.expect('$');
            const v = this.name();
            this.expect(':');
            this.typeRef();
            let def: Value | null = null;
            if (this.peek('=')) {
              this.next();
              def = this.value(true);
            }
            this.directives();
            vars.push({ name: v, def });
          }
          this.next();
        }
        this.directives();
        doc.ops.push({ op: opTok, name, vars, sel: this.selSet() });
      }
    }
    return doc;
  }
  typeRef(): string {
    let s: string;
    if (this.peek('[')) {
      this.next();
      s = `[${this.typeRef()}]`;
      this.expect(']');
    } else s = this.name();
    if (this.peek('!')) {
      this.next();
      s += '!';
    }
    return s;
  }
  directives(): void {
    while (this.peek('@')) {
      this.next();
      this.name();
      if (this.peek('(')) this.args();
    }
  }
  selSet(): Selection[] {
    this.expect('{');
    const out: Selection[] = [];
    while (!this.peek('}')) {
      if (this.t[this.p].k === 'eof') throw new Error('Unexpected <EOF>');
      if (this.peek('...')) {
        this.next();
        if (this.peek('on')) {
          this.next();
          const on = this.name();
          this.directives();
          out.push({ kind: 'inline', on, sel: this.selSet() });
        } else if (this.peek('{') || this.peek('@')) {
          this.directives();
          out.push({ kind: 'inline', on: null, sel: this.selSet() });
        } else {
          out.push({ kind: 'spread', name: this.name() });
          this.directives();
        }
        continue;
      }
      let name = this.name();
      let alias: string | null = null;
      if (this.peek(':')) {
        this.next();
        alias = name;
        name = this.name();
      }
      const args = this.peek('(') ? this.args() : [];
      this.directives();
      const sel = this.peek('{') ? this.selSet() : null;
      out.push({ kind: 'field', alias, name, args, sel });
    }
    this.next();
    return out;
  }
  args(): Array<[string, Value]> {
    this.expect('(');
    const out: Array<[string, Value]> = [];
    while (!this.peek(')')) {
      const n = this.name();
      this.expect(':');
      out.push([n, this.value(false)]);
    }
    this.next();
    return out;
  }
  value(isConst: boolean): Value {
    const tok = this.next();
    if (tok.k === 'punct') {
      if (tok.v === '$' && !isConst) return { kind: 'var', name: this.name() };
      if (tok.v === '[') {
        const items: Value[] = [];
        while (!this.peek(']')) items.push(this.value(isConst));
        this.next();
        return { kind: 'list', items };
      }
      if (tok.v === '{') {
        const fields: Array<[string, Value]> = [];
        while (!this.peek('}')) {
          const n = this.name();
          this.expect(':');
          fields.push([n, this.value(isConst)]);
        }
        this.next();
        return { kind: 'object', fields };
      }
      throw new Error(`Unexpected ${JSON.stringify(tok.v)}`);
    }
    if (tok.k === 'int') return { kind: 'int', value: Number(tok.v) };
    if (tok.k === 'float') return { kind: 'float', value: Number(tok.v) };
    if (tok.k === 'string') return { kind: 'string', value: tok.v };
    if (tok.k === 'name') {
      if (tok.v === 'true' || tok.v === 'false') return { kind: 'boolean', value: tok.v === 'true' };
      if (tok.v === 'null') return { kind: 'null', value: null };
      return { kind: 'enum', value: tok.v };
    }
    throw new Error('Unexpected <EOF>');
  }
}

// ------------------------------------------------------------------ schema (SDL)

const SDL = `
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
`;

interface FieldDef { name: string; type: string; args: Array<{ name: string; type: string }> }
interface TypeDef {
  name: string;
  kind: 'OBJECT' | 'INTERFACE' | 'UNION' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR';
  fields: Map<string, FieldDef>;
  interfaces: string[];
  values: string[];
  members: string[];
}

function parseSdl(src: string): Map<string, TypeDef> {
  const p = new Parser(src);
  const types = new Map<string, TypeDef>();
  const fieldList = (): Map<string, FieldDef> => {
    const out = new Map<string, FieldDef>();
    p.expect('{');
    while (!p.peek('}')) {
      const name = p.name();
      const args: FieldDef['args'] = [];
      if (p.peek('(')) {
        p.next();
        while (!p.peek(')')) {
          const an = p.name();
          p.expect(':');
          args.push({ name: an, type: p.typeRef() });
        }
        p.next();
      }
      p.expect(':');
      out.set(name, { name, type: p.typeRef(), args });
    }
    p.next();
    return out;
  };
  while (p.t[p.p].k !== 'eof') {
    const kw = p.name();
    const name = p.name();
    const def: TypeDef = { name, kind: 'OBJECT', fields: new Map(), interfaces: [], values: [], members: [] };
    if (kw === 'type' || kw === 'interface' || kw === 'input') {
      def.kind = kw === 'type' ? 'OBJECT' : kw === 'interface' ? 'INTERFACE' : 'INPUT_OBJECT';
      if (p.peek('implements')) {
        p.next();
        def.interfaces.push(p.name());
        while (p.peek('&')) {
          p.next();
          def.interfaces.push(p.name());
        }
      }
      def.fields = fieldList();
    } else if (kw === 'enum') {
      def.kind = 'ENUM';
      p.expect('{');
      while (!p.peek('}')) def.values.push(p.name());
      p.next();
    } else if (kw === 'union') {
      def.kind = 'UNION';
      p.expect('=');
      def.members.push(p.name());
      while (p.peek('|')) {
        p.next();
        def.members.push(p.name());
      }
    } else if (kw === 'scalar') {
      def.kind = 'SCALAR';
    } else throw new Error(`bad SDL keyword ${kw}`);
    types.set(name, def);
  }
  return types;
}

const TYPES = parseSdl(SDL);

// ------------------------------------------------------------------ introspection objects

interface TypeRefObj { __t: '__Type'; kind: string; name: string | null; ofType: TypeRefObj | null; def: TypeDef | null }

function typeRefObj(ref: string): TypeRefObj {
  if (ref.endsWith('!')) return { __t: '__Type', kind: 'NON_NULL', name: null, ofType: typeRefObj(ref.slice(0, -1)), def: null };
  if (ref.startsWith('[')) return { __t: '__Type', kind: 'LIST', name: null, ofType: typeRefObj(ref.slice(1, -1)), def: null };
  const def = TYPES.get(ref) ?? null;
  return { __t: '__Type', kind: def?.kind ?? 'SCALAR', name: ref, ofType: null, def };
}

const INTROSPECTION_TYPES: Record<string, Record<string, string>> = {
  __Schema: { queryType: '__Type!', mutationType: '__Type', subscriptionType: '__Type', types: '[__Type!]!', directives: '[__Directive!]!', description: 'String' },
  __Type: { kind: '__TypeKind!', name: 'String', description: 'String', fields: '[__Field!]', interfaces: '[__Type!]', possibleTypes: '[__Type!]', enumValues: '[__EnumValue!]', inputFields: '[__InputValue!]', ofType: '__Type', specifiedByURL: 'String' },
  __Field: { name: 'String!', description: 'String', args: '[__InputValue!]!', type: '__Type!', isDeprecated: 'Boolean!', deprecationReason: 'String' },
  __InputValue: { name: 'String!', description: 'String', type: '__Type!', defaultValue: 'String', isDeprecated: 'Boolean!', deprecationReason: 'String' },
  __EnumValue: { name: 'String!', description: 'String', isDeprecated: 'Boolean!', deprecationReason: 'String' },
  __Directive: { name: 'String!', description: 'String', locations: '[String!]!', args: '[__InputValue!]!', isRepeatable: 'Boolean!' },
};

function introspect(typeName: string, field: string, parent: Record<string, unknown>): unknown {
  if (typeName === '__Schema') {
    if (field === 'queryType') return typeRefObj('Query');
    if (field === 'mutationType') return typeRefObj('Mutation');
    if (field === 'types') return [...TYPES.keys()].map(typeRefObj);
    if (field === 'directives') return [];
    return null;
  }
  if (typeName === '__Type') {
    const t = parent as unknown as TypeRefObj;
    const def = t.def;
    switch (field) {
      case 'kind': return t.kind;
      case 'name': return t.name;
      case 'ofType': return t.ofType;
      case 'description': return null;
      case 'specifiedByURL': return null;
      case 'fields':
        if (def === null || (def.kind !== 'OBJECT' && def.kind !== 'INTERFACE')) return null;
        return [...def.fields.values()].map((f) => ({ __t: '__Field', name: f.name, description: null, args: f.args.map((a) => ({ __t: '__InputValue', name: a.name, description: null, type: typeRefObj(a.type), defaultValue: null, isDeprecated: false, deprecationReason: null })), type: typeRefObj(f.type), isDeprecated: false, deprecationReason: null }));
      case 'inputFields':
        if (def === null || def.kind !== 'INPUT_OBJECT') return null;
        return [...def.fields.values()].map((f) => ({ __t: '__InputValue', name: f.name, description: null, type: typeRefObj(f.type), defaultValue: null, isDeprecated: false, deprecationReason: null }));
      case 'interfaces':
        return def === null || def.kind !== 'OBJECT' ? (def?.kind === 'INTERFACE' ? [] : null) : def.interfaces.map(typeRefObj);
      case 'possibleTypes':
        if (def === null) return null;
        if (def.kind === 'UNION') return def.members.map(typeRefObj);
        if (def.kind === 'INTERFACE') return [...TYPES.values()].filter((x) => x.interfaces.includes(def.name)).map((x) => typeRefObj(x.name));
        return null;
      case 'enumValues':
        if (def === null || def.kind !== 'ENUM') return null;
        return def.values.map((v) => ({ __t: '__EnumValue', name: v, description: null, isDeprecated: false, deprecationReason: null }));
      default: return null;
    }
  }
  return (parent as Record<string, unknown>)[field] ?? null;
}

// ------------------------------------------------------------------ resolvers

class GqlError extends Error {
  type: string;
  constructor(message: string, type = 'UNPROCESSABLE') {
    super(message);
    this.type = type;
  }
}

type Args = Record<string, unknown>;
type Resolver = (parent: any, args: Args) => unknown;

function connection<T>(items: T[], args: Args): Record<string, unknown> {
  let start = 0;
  if (typeof args.after === 'string' && args.after !== '') {
    const idx = decodeCursor(args.after);
    if (idx === null) throw new GqlError(`\`${args.after}\` does not appear to be a valid cursor.`, 'INVALID_CURSOR_ARGUMENTS');
    start = idx + 1;
  }
  let end = items.length;
  if (typeof args.before === 'string' && args.before !== '') {
    const idx = decodeCursor(args.before);
    if (idx !== null) end = Math.min(end, idx);
  }
  let from = start;
  let to = end;
  const first = typeof args.first === 'number' ? args.first : null;
  const last = typeof args.last === 'number' ? args.last : null;
  if (first !== null) {
    if (first < 0 || first > 100) throw new GqlError('Requesting more than 100 records on the connection is not allowed.', 'EXCESSIVE_PAGINATION');
    to = Math.min(to, from + first);
  }
  if (last !== null) from = Math.max(from, to - last);
  if (first === null && last === null) to = Math.min(to, from + 100);
  const slice = items.slice(from, to);
  const edges = slice.map((node, k) => ({ cursor: cursor(from + k), node }));
  return {
    nodes: slice,
    edges,
    totalCount: items.length,
    pageInfo: {
      hasNextPage: to < end,
      hasPreviousPage: from > 0,
      startCursor: slice.length > 0 ? cursor(from) : null,
      endCursor: slice.length > 0 ? cursor(to - 1) : null,
    },
  };
}

function cursor(i: number): string {
  return Buffer.from(`cursor:v2:${i}`).toString('base64');
}
function decodeCursor(c: string): number | null {
  const m = /^cursor:v2:(\d+)$/.exec(Buffer.from(c, 'base64').toString('utf8'));
  return m === null ? null : Number(m[1]);
}

const ORIGIN = { value: 'http://localhost' };

function repoUrl(r: Repo): string {
  return `${ORIGIN.value}/${r.owner}/${r.name}`;
}

function ownRepo(owner: unknown, name: unknown): Repo | null {
  if (typeof owner !== 'string' || typeof name !== 'string') return null;
  const r = S.getRepo(owner, name);
  return r !== undefined && r.owner.toLowerCase() === owner.toLowerCase() ? r : null;
}

function nodeById(id: unknown): unknown {
  if (typeof id !== 'string') return null;
  const d = S.decodeGlobalId(id);
  if (d === null) return null;
  if (d.kind === 'R') return S.repoById(d.id) ?? null;
  if (d.kind === 'I') return S.issuesById.get(d.id) ?? null;
  if (d.kind === 'PR') {
    for (const i of S.issuesById.values()) if (i.pull !== null && i.pull.id === d.id) return i.pull;
    return null;
  }
  if (d.kind === 'IC') return S.commentsById.get(d.id) ?? null;
  if (d.kind === 'U' && d.id === VIEWER.id) return VIEWER_NODE;
  return null;
}

const VIEWER_NODE = { __viewer: true };

function typeOfValue(v: any): string {
  if (v === VIEWER_NODE) return 'User';
  if (v.__t !== undefined) return v.__t;
  if (v.objects instanceof Map && v.refs instanceof Map) return 'Repository';
  if ('pull' in v && 'seq' in v) return v.pull !== null ? 'Issue' : 'Issue';
  if ('issue' in v && 'head' in v) return 'PullRequest';
  if ('issue' in v && 'body' in v) return 'IssueComment';
  if ('color' in v) return 'Label';
  return 'Unknown';
}

function issueStates(args: Args): Set<string> | null {
  const states = args.states ?? (args.filterBy as Args | undefined)?.states;
  return Array.isArray(states) && states.length > 0 ? new Set(states.map(String)) : null;
}

function sortIssues<T extends { seq: number; updated: string; comments: unknown[] }>(items: T[], order: unknown, defaultDir: 'ASC' | 'DESC'): T[] {
  const o = (order ?? {}) as Args;
  const field = typeof o.field === 'string' ? o.field : 'CREATED_AT';
  const dir = typeof o.direction === 'string' ? o.direction : defaultDir;
  const key = (i: T): number => (field === 'UPDATED_AT' ? Date.parse(i.updated) : field === 'COMMENTS' ? i.comments.length : i.seq);
  return items.slice().sort((a, b) => (key(a) - key(b) || a.seq - b.seq) * (dir === 'DESC' ? -1 : 1));
}

function pullState(p: Pull): string {
  return p.merged ? 'MERGED' : p.issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function issueByNumber(r: Repo, n: unknown): Issue | null {
  const i = typeof n === 'number' ? r.byNumber.get(n) : undefined;
  return i === undefined ? null : i;
}

const R: Record<string, Record<string, Resolver>> = {
  Query: {
    viewer: () => VIEWER_NODE,
    repository: (_p, a) => ownRepo(a.owner, a.name),
    repositoryOwner: (_p, a) => (typeof a.login === 'string' && a.login.toLowerCase() === VIEWER.login.toLowerCase() ? VIEWER_NODE : null),
    user: (_p, a) => (typeof a.login === 'string' && a.login.toLowerCase() === VIEWER.login.toLowerCase() ? VIEWER_NODE : null),
    node: (_p, a) => nodeById(a.id),
    nodes: (_p, a) => (Array.isArray(a.ids) ? a.ids.map(nodeById) : []),
    rateLimit: () => ({ __t: 'RateLimit', limit: 5000, cost: 1, remaining: 5000, resetAt: '2026-01-01T00:00:00Z', used: 0 }),
    search: (_p, a) => {
      const q = String(a.query ?? '');
      const terms = q.split(/\s+/).filter((t) => t !== '' && !t.includes(':')).map((t) => t.toLowerCase());
      const repoMatch = /repo:([^\s/]+)\/(\S+)/.exec(q);
      const isOnly = /is:(issue|pr|pull-request)/.exec(q)?.[1];
      const stateOnly = /is:(open|closed)/.exec(q)?.[1];
      let items: unknown[] = [];
      if (a.type === 'REPOSITORY') items = S.listRepos(VIEWER.login).filter((r) => terms.every((t) => r.name.toLowerCase().includes(t)));
      else if (a.type === 'USER') items = [VIEWER_NODE];
      else {
        const repos = repoMatch ? [ownRepo(repoMatch[1], repoMatch[2])].filter((x): x is Repo => x !== null) : S.listRepos(VIEWER.login);
        for (const r of repos) for (const i of r.issues) {
          if (isOnly === 'issue' && i.pull !== null) continue;
          if ((isOnly === 'pr' || isOnly === 'pull-request') && i.pull === null) continue;
          if (stateOnly !== undefined && i.state !== stateOnly) continue;
          if (!terms.every((t) => `${i.title} ${i.body}`.toLowerCase().includes(t))) continue;
          items.push(i.pull ?? i);
        }
      }
      const c = connection(items, a);
      return { ...c, issueCount: a.type === 'REPOSITORY' || a.type === 'USER' ? 0 : items.length, repositoryCount: a.type === 'REPOSITORY' ? items.length : 0 };
    },
  },
  Mutation: {
    createIssue: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const r = nodeById(input.repositoryId);
      if (r === null || typeOfValue(r) !== 'Repository') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.repositoryId)}'`, 'NOT_FOUND');
      const repo = r as Repo;
      const labelNames: string[] = [];
      if (Array.isArray(input.labelIds)) for (const id of input.labelIds) {
        const lab = [...repo.labels.values()].find((l) => S.globalId('LA', l.id) === id);
        if (lab === undefined) throw new GqlError(`Could not resolve to a node with the global id of '${String(id)}'`, 'NOT_FOUND');
        labelNames.push(lab.name);
      }
      return { issue: createIssueChecked(repo, input.title, input.body, labelNames), clientMutationId: input.clientMutationId ?? null };
    },
    updateIssue: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const i = nodeById(input.id) as Issue | null;
      if (i === null || typeOfValue(i) !== 'Issue') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.id)}'`, 'NOT_FOUND');
      if (typeof input.title === 'string') i.title = input.title;
      if (typeof input.body === 'string') i.body = input.body;
      if (input.state === 'OPEN' || input.state === 'CLOSED') S.setIssueState(i, input.state === 'OPEN' ? 'open' : 'closed');
      i.json = undefined;
      return { issue: i, clientMutationId: input.clientMutationId ?? null };
    },
    closeIssue: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const i = nodeById(input.issueId) as Issue | null;
      if (i === null || typeOfValue(i) !== 'Issue') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.issueId)}'`, 'NOT_FOUND');
      S.setIssueState(i, 'closed');
      return { issue: i, clientMutationId: input.clientMutationId ?? null };
    },
    reopenIssue: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const i = nodeById(input.issueId) as Issue | null;
      if (i === null || typeOfValue(i) !== 'Issue') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.issueId)}'`, 'NOT_FOUND');
      S.setIssueState(i, 'open');
      return { issue: i, clientMutationId: input.clientMutationId ?? null };
    },
    addComment: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const subject = nodeById(input.subjectId);
      const t = subject === null ? '' : typeOfValue(subject);
      if (t !== 'Issue' && t !== 'PullRequest') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.subjectId)}'`, 'NOT_FOUND');
      if (typeof input.body !== 'string' || input.body === '') throw new GqlError('Body cannot be blank');
      const issue = t === 'Issue' ? (subject as Issue) : (subject as Pull).issue;
      const c = S.addComment(issue, input.body);
      const edge = { __t: 'IssueCommentEdge', cursor: cursor(issue.comments.length - 1), node: c };
      return { commentEdge: edge, timelineEdge: edge, subject, clientMutationId: input.clientMutationId ?? null };
    },
    createPullRequest: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const r = nodeById(input.repositoryId);
      if (r === null || typeOfValue(r) !== 'Repository') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.repositoryId)}'`, 'NOT_FOUND');
      return { pullRequest: createPullChecked(r as Repo, input.title, input.body, input.headRefName, input.baseRefName), clientMutationId: input.clientMutationId ?? null };
    },
    mergePullRequest: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const p = nodeById(input.pullRequestId) as Pull | null;
      if (p === null || typeOfValue(p) !== 'PullRequest') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.pullRequestId)}'`, 'NOT_FOUND');
      const method = typeof input.mergeMethod === 'string' ? input.mergeMethod.toLowerCase() : 'merge';
      const res = S.mergePull(p, method, typeof input.commitHeadline === 'string' ? input.commitHeadline : undefined, typeof input.commitBody === 'string' ? input.commitBody : undefined);
      if (!res.ok) throw new GqlError(res.message);
      return { pullRequest: p, clientMutationId: input.clientMutationId ?? null };
    },
    closePullRequest: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      const p = nodeById(input.pullRequestId) as Pull | null;
      if (p === null || typeOfValue(p) !== 'PullRequest') throw new GqlError(`Could not resolve to a node with the global id of '${String(input.pullRequestId)}'`, 'NOT_FOUND');
      if (!p.merged) S.setIssueState(p.issue, 'closed');
      return { pullRequest: p, clientMutationId: input.clientMutationId ?? null };
    },
    createRepository: (_p, a) => {
      const input = (a.input ?? {}) as Args;
      return { repository: createRepoChecked({ name: input.name, description: input.description, private: input.visibility === 'PRIVATE' }), clientMutationId: input.clientMutationId ?? null };
    },
  },
  User: {
    id: () => S.globalId('U', VIEWER.id),
    databaseId: () => VIEWER.id,
    login: () => VIEWER.login,
    name: () => VIEWER.name,
    email: () => VIEWER.email,
    url: () => `${ORIGIN.value}/${VIEWER.login}`,
    avatarUrl: () => `${ORIGIN.value}/${VIEWER.login}.png`,
    repositories: (_p, a) => connection(S.listRepos(VIEWER.login), a),
  },
  Repository: {
    id: (r: Repo) => S.globalId('R', r.id),
    databaseId: (r: Repo) => r.id,
    name: (r: Repo) => r.name,
    nameWithOwner: (r: Repo) => `${r.owner}/${r.name}`,
    owner: () => VIEWER_NODE,
    description: (r: Repo) => r.description,
    url: (r: Repo) => repoUrl(r),
    sshUrl: (r: Repo) => `git@localhost:${r.owner}/${r.name}.git`,
    isPrivate: (r: Repo) => r.private,
    visibility: (r: Repo) => (r.private ? 'PRIVATE' : 'PUBLIC'),
    isFork: () => false,
    isArchived: () => false,
    isEmpty: (r: Repo) => r.refs.size === 0,
    createdAt: (r: Repo) => r.created,
    updatedAt: (r: Repo) => r.updated,
    pushedAt: (r: Repo) => r.pushed,
    defaultBranchRef: (r: Repo) => {
      const sha = r.refs.get(`refs/heads/${r.defaultBranch}`);
      return sha === undefined ? null : { __t: 'Ref', repo: r, refName: `refs/heads/${r.defaultBranch}`, sha };
    },
    viewerPermission: () => 'ADMIN',
    hasIssuesEnabled: () => true,
    hasWikiEnabled: () => false,
    hasProjectsEnabled: () => false,
    viewerCanAdminister: () => true,
    mergeCommitAllowed: () => true,
    squashMergeAllowed: () => true,
    rebaseMergeAllowed: () => false,
    issue: (r: Repo, a) => {
      const i = issueByNumber(r, a.number);
      if (i === null || i.pull !== null) throw new GqlError(`Could not resolve to an Issue with the number of ${String(a.number)}.`, 'NOT_FOUND');
      return i;
    },
    issueOrPullRequest: (r: Repo, a) => {
      const i = issueByNumber(r, a.number);
      if (i === null) throw new GqlError(`Could not resolve to an issue or pull request with the number of ${String(a.number)}.`, 'NOT_FOUND');
      return i.pull ?? i;
    },
    issues: (r: Repo, a) => {
      const states = issueStates(a);
      const labels = (a.labels ?? (a.filterBy as Args | undefined)?.labels) as string[] | undefined;
      const items = r.issues.filter((i) => i.pull === null && (states === null || states.has(i.state.toUpperCase())) && (!Array.isArray(labels) || labels.every((l) => i.labels.some((x) => x.name === l))));
      return connection(sortIssues(items, a.orderBy, 'ASC'), a);
    },
    pullRequest: (r: Repo, a) => {
      const i = issueByNumber(r, a.number);
      if (i === null || i.pull === null) throw new GqlError(`Could not resolve to a PullRequest with the number of ${String(a.number)}.`, 'NOT_FOUND');
      return i.pull;
    },
    pullRequests: (r: Repo, a) => {
      const states = Array.isArray(a.states) && a.states.length > 0 ? new Set(a.states.map(String)) : null;
      const items = r.issues.filter((i) => i.pull !== null && (states === null || states.has(pullState(i.pull))) && (typeof a.headRefName !== 'string' || i.pull.head === a.headRefName) && (typeof a.baseRefName !== 'string' || i.pull.base === a.baseRefName));
      return connection(sortIssues(items, a.orderBy, 'ASC').map((i) => i.pull as Pull), a);
    },
    ref: (r: Repo, a) => {
      const q = String(a.qualifiedName ?? '');
      const full = q.startsWith('refs/') ? q : `refs/heads/${q}`;
      const sha = r.refs.get(full);
      return sha === undefined ? null : { __t: 'Ref', repo: r, refName: full, sha };
    },
    refs: (r: Repo, a) => {
      const prefix = String(a.refPrefix ?? 'refs/heads/');
      const items = [...r.refs.entries()].filter(([k]) => k.startsWith(prefix)).sort().map(([k, sha]) => ({ __t: 'Ref', repo: r, refName: k, sha, prefix }));
      return connection(items, a);
    },
    label: (r: Repo, a) => r.labels.get(String(a.name ?? '').toLowerCase()) ?? null,
    labels: (r: Repo, a) => connection([...r.labels.values()], a),
  },
  Issue: {
    id: (i: Issue) => S.globalId('I', i.id),
    databaseId: (i: Issue) => i.id,
    number: (i: Issue) => i.number,
    title: (i: Issue) => i.title,
    body: (i: Issue) => i.body,
    bodyText: (i: Issue) => i.body,
    state: (i: Issue) => i.state.toUpperCase(),
    stateReason: (i: Issue) => (i.state === 'closed' ? 'COMPLETED' : null),
    url: (i: Issue) => `${repoUrl(i.repo)}/issues/${i.number}`,
    createdAt: (i: Issue) => i.created,
    updatedAt: (i: Issue) => i.updated,
    closed: (i: Issue) => i.state === 'closed',
    closedAt: (i: Issue) => i.closed,
    author: () => VIEWER_NODE,
    repository: (i: Issue) => i.repo,
    labels: (i: Issue, a) => connection(i.labels, a),
    comments: (i: Issue, a) => connection(i.comments, a),
    viewerCanUpdate: () => true,
    locked: () => false,
  },
  PullRequest: {
    id: (p: Pull) => S.globalId('PR', p.id),
    databaseId: (p: Pull) => p.id,
    number: (p: Pull) => p.issue.number,
    title: (p: Pull) => p.issue.title,
    body: (p: Pull) => p.issue.body,
    state: (p: Pull) => pullState(p),
    url: (p: Pull) => `${repoUrl(p.issue.repo)}/pull/${p.issue.number}`,
    createdAt: (p: Pull) => p.issue.created,
    updatedAt: (p: Pull) => p.issue.updated,
    closed: (p: Pull) => p.issue.state === 'closed',
    closedAt: (p: Pull) => p.issue.closed,
    merged: (p: Pull) => p.merged,
    mergedAt: (p: Pull) => p.mergedAt,
    mergeable: (p: Pull) => (p.issue.state !== 'open' ? 'UNKNOWN' : S.isMergeable(p) ? 'MERGEABLE' : 'CONFLICTING'),
    headRefName: (p: Pull) => p.head,
    baseRefName: (p: Pull) => p.base,
    headRefOid: (p: Pull) => S.pullShas(p).head,
    baseRefOid: (p: Pull) => S.pullShas(p).base,
    isDraft: () => false,
    author: () => VIEWER_NODE,
    repository: (p: Pull) => p.issue.repo,
    headRepository: (p: Pull) => p.issue.repo,
    baseRepository: (p: Pull) => p.issue.repo,
    mergeCommit: (p: Pull) => (p.mergeSha === null ? null : { __t: 'Commit', repo: p.issue.repo, sha: p.mergeSha }),
    additions: (p: Pull) => S.pullFiles(p).reduce((s, f) => s + f.additions, 0),
    deletions: (p: Pull) => S.pullFiles(p).reduce((s, f) => s + f.deletions, 0),
    changedFiles: (p: Pull) => S.pullFiles(p).length,
    files: (p: Pull, a) => connection(S.pullFiles(p).map((f) => ({ __t: 'PullRequestChangedFile', path: f.path, additions: f.additions, deletions: f.deletions, changeType: f.status === 'added' ? 'ADDED' : f.status === 'removed' ? 'DELETED' : 'MODIFIED' })), a),
    labels: (p: Pull, a) => connection(p.issue.labels, a),
    comments: (p: Pull, a) => connection(p.issue.comments, a),
    locked: () => false,
  },
  IssueComment: {
    id: (c: S.Comment) => S.globalId('IC', c.id),
    databaseId: (c: S.Comment) => c.id,
    body: (c: S.Comment) => c.body,
    author: () => VIEWER_NODE,
    createdAt: (c: S.Comment) => c.created,
    url: (c: S.Comment) => `${repoUrl(c.issue.repo)}/issues/${c.issue.number}#issuecomment-${c.id}`,
    issue: (c: S.Comment) => c.issue,
  },
  Label: {
    id: (l: S.Label) => S.globalId('LA', l.id),
    name: (l: S.Label) => l.name,
    color: (l: S.Label) => l.color,
    description: (l: S.Label) => l.description,
  },
  Ref: {
    id: (x: { repo: Repo; refName: string }) => `REF_${Buffer.from(`${x.repo.id}:${x.refName}`).toString('base64url')}`,
    name: (x: { refName: string }) => x.refName.replace(/^refs\/(heads|tags)\//, ''),
    prefix: (x: { refName: string }) => (/^refs\/(heads|tags)\//.exec(x.refName)?.[0] ?? 'refs/'),
    target: (x: { repo: Repo; sha: string }) => ({ __t: 'Commit', repo: x.repo, sha: x.sha }),
    repository: (x: { repo: Repo }) => x.repo,
  },
  Commit: {
    id: (x: { sha: string }) => `C_${Buffer.from(x.sha, 'hex').toString('base64url')}`,
    oid: (x: { sha: string }) => x.sha,
    message: (x: { repo: Repo; sha: string }) => (S.commitOf(x.repo, x.sha)?.message ?? '').replace(/\n$/, ''),
    messageHeadline: (x: { repo: Repo; sha: string }) => (S.commitOf(x.repo, x.sha)?.message ?? '').split('\n')[0],
    committedDate: () => '2026-01-01T00:00:00Z',
    url: (x: { repo: Repo; sha: string }) => `${repoUrl(x.repo)}/commit/${x.sha}`,
  },
};

// ------------------------------------------------------------------ executor

interface Exec {
  doc: Doc;
  vars: Args;
  errors: Array<{ message: string; type?: string; path?: Array<string | number>; locations?: unknown[] }>;
  invalid: boolean;
}

function valueOf(v: Value, ex: Exec): unknown {
  switch (v.kind) {
    case 'var': return ex.vars[v.name] ?? null;
    case 'list': return v.items.map((x) => valueOf(x, ex));
    case 'object': {
      const o: Args = {};
      for (const [k, x] of v.fields) o[k] = valueOf(x, ex);
      return o;
    }
    default: return v.value;
  }
}

function namedType(ref: string): string {
  return ref.replace(/[[\]!]/g, '');
}

function collect(ex: Exec, typeName: string, sel: Selection[], out: Map<string, Field[]>, visited: Set<string>): void {
  for (const s of sel) {
    if (s.kind === 'field') {
      const k = s.alias ?? s.name;
      const list = out.get(k);
      if (list === undefined) out.set(k, [s]);
      else list.push(s);
    } else if (s.kind === 'inline') {
      if (s.on === null || typeMatches(typeName, s.on)) collect(ex, typeName, s.sel, out, visited);
    } else {
      const f = ex.doc.frags.get(s.name);
      if (f === undefined) {
        ex.errors.push({ message: `Fragment ${s.name} was used, but not defined` });
        ex.invalid = true;
        continue;
      }
      if (visited.has(s.name)) continue;
      visited.add(s.name);
      if (typeMatches(typeName, f.on)) collect(ex, typeName, f.sel, out, visited);
    }
  }
}

function typeMatches(concrete: string, cond: string): boolean {
  if (concrete === cond) return true;
  const def = TYPES.get(cond);
  if (def === undefined) return false;
  if (def.kind === 'UNION') return def.members.includes(concrete);
  if (def.kind === 'INTERFACE') return TYPES.get(concrete)?.interfaces.includes(cond) ?? false;
  return false;
}

function fieldDefOf(typeName: string, field: string): { type: string } | null {
  const intro = INTROSPECTION_TYPES[typeName];
  if (intro !== undefined) return intro[field] === undefined ? null : { type: intro[field] };
  const def = TYPES.get(typeName);
  if (def === undefined) return null;
  const f = def.fields.get(field);
  return f === undefined ? null : { type: f.type };
}

function selectObject(ex: Exec, typeName: string, value: any, sel: Selection[], path: Array<string | number>): Record<string, unknown> {
  const fields = new Map<string, Field[]>();
  collect(ex, typeName, sel, fields, new Set());
  const out: Record<string, unknown> = {};
  for (const [key, list] of fields) {
    const f = list[0];
    const subSel: Selection[] = [];
    for (const x of list) if (x.sel !== null) subSel.push(...x.sel);
    out[key] = resolveField(ex, typeName, value, f, subSel.length > 0 ? subSel : null, [...path, key]);
  }
  return out;
}

function resolveField(ex: Exec, typeName: string, value: any, f: Field, sel: Selection[] | null, path: Array<string | number>): unknown {
  if (f.name === '__typename') return typeName;
  if (typeName === 'Query' && f.name === '__type') {
    const args: Args = {};
    for (const [k, v] of f.args) args[k] = valueOf(v, ex);
    const name = String(args.name ?? '');
    const t = TYPES.has(name) ? typeRefObj(name) : INTROSPECTION_TYPES[name] !== undefined ? { __t: '__Type', kind: 'OBJECT', name, ofType: null, def: null } : null;
    return t === null || sel === null ? null : selectObject(ex, '__Type', t, sel, path);
  }
  if (typeName === 'Query' && f.name === '__schema') {
    return sel === null ? null : selectObject(ex, '__Schema', {}, sel, path);
  }
  const def = fieldDefOf(typeName, f.name);
  if (def === null) {
    ex.errors.push({ message: `Field '${f.name}' doesn't exist on type '${typeName}'`, type: 'undefinedField', path: path.filter((p) => typeof p === 'string') as string[] });
    ex.invalid = true;
    return null;
  }
  const args: Args = {};
  for (const [k, v] of f.args) args[k] = valueOf(v, ex);
  let raw: unknown;
  try {
    if (typeName.startsWith('__')) raw = introspect(typeName, f.name, value);
    else {
      const res = R[typeName]?.[f.name];
      raw = res !== undefined ? res(value, args) : value?.[f.name] ?? null;
    }
  } catch (err) {
    const e = err as Error & { type?: string; status?: number };
    ex.errors.push({ message: e.message, type: e instanceof GqlError ? e.type : e instanceof HttpError ? 'UNPROCESSABLE' : 'INTERNAL', path });
    return null;
  }
  return complete(ex, def.type, raw, sel, path, f.name);
}

function complete(ex: Exec, ref: string, raw: unknown, sel: Selection[] | null, path: Array<string | number>, fieldName: string): unknown {
  if (raw === null || raw === undefined) return null;
  const inner = ref.endsWith('!') ? ref.slice(0, -1) : ref;
  if (inner.startsWith('[')) {
    if (!Array.isArray(raw)) return null;
    const itemRef = inner.slice(1, -1);
    return raw.map((item, k) => complete(ex, itemRef, item, sel, [...path, k], fieldName));
  }
  const name = namedType(inner);
  const intro = INTROSPECTION_TYPES[name];
  const def = TYPES.get(name);
  const isLeaf = intro === undefined && (def === undefined || def.kind === 'SCALAR' || def.kind === 'ENUM') && name !== '__TypeKind' ? true : name === '__TypeKind';
  if (isLeaf) {
    if (sel !== null) {
      ex.errors.push({ message: `Selections can't be made on scalars (field '${fieldName}' returns ${name} but has selections [${sel.map((s) => (s.kind === 'field' ? s.name : '...')).join(', ')}])` });
      ex.invalid = true;
    }
    return raw;
  }
  if (sel === null) {
    ex.errors.push({ message: `Field must have selections (field '${fieldName}' returns ${name} but has no selections. Did you mean '${fieldName} { ... }'?)` });
    ex.invalid = true;
    return null;
  }
  let concrete = name;
  if (def !== undefined && (def.kind === 'INTERFACE' || def.kind === 'UNION')) concrete = typeOfValue(raw);
  else if (def === undefined && intro === undefined) concrete = typeOfValue(raw);
  return selectObject(ex, concrete, raw, sel, path);
}

export interface GqlRequest {
  query?: unknown;
  variables?: unknown;
  operationName?: unknown;
}

export function executeGraphql(req: GqlRequest, origin: string): Record<string, unknown> {
  ORIGIN.value = origin;
  if (typeof req !== 'object' || req === null || typeof req.query !== 'string') {
    return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  }
  let doc: Doc;
  try {
    doc = new Parser(req.query).doc();
  } catch (err) {
    return { errors: [{ message: `Parse error on ${(err as Error).message}`, type: 'PARSE' }] };
  }
  const opName = typeof req.operationName === 'string' && req.operationName !== '' ? req.operationName : null;
  const op = opName === null ? (doc.ops.length === 1 ? doc.ops[0] : undefined) : doc.ops.find((o) => o.name === opName);
  if (op === undefined) {
    return { errors: [{ message: opName === null ? 'An operation name is required when the document holds more than one operation' : `No operation named "${opName}"` }] };
  }
  const given = typeof req.variables === 'object' && req.variables !== null ? (req.variables as Args) : {};
  const vars: Args = {};
  for (const v of op.vars) {
    if (v.name in given) vars[v.name] = given[v.name];
    else if (v.def !== null) vars[v.name] = valueOf(v.def, { doc, vars: {}, errors: [], invalid: false });
  }
  const ex: Exec = { doc, vars, errors: [], invalid: false };
  const root = op.op === 'mutation' ? 'Mutation' : 'Query';
  // Validate the whole document first (a dry walk over a placeholder) so an unknown field is reported
  // without running any mutation.
  validate(ex, root, op.sel);
  if (ex.invalid) return { errors: ex.errors };
  const data = selectObject(ex, root, {}, op.sel, []);
  return ex.errors.length > 0 ? { data, errors: ex.errors } : { data };
}

/** Static check of every field against the schema (unknown fields and leaf/selection mismatches). */
function validate(ex: Exec, typeName: string, sel: Selection[]): void {
  const fields = new Map<string, Field[]>();
  collectStatic(ex, typeName, sel, fields);
  for (const list of fields.values()) {
    for (const f of list) {
      if (f.name === '__typename') continue;
      if (f.name === '__type' || f.name === '__schema') continue;
      const def = fieldDefOf(typeName, f.name);
      if (def === null) {
        ex.errors.push({ message: `Field '${f.name}' doesn't exist on type '${typeName}'`, type: 'undefinedField' });
        ex.invalid = true;
        continue;
      }
      const name = namedType(def.type);
      const tdef = TYPES.get(name);
      const leaf = INTROSPECTION_TYPES[name] === undefined && (tdef === undefined || tdef.kind === 'SCALAR' || tdef.kind === 'ENUM');
      if (leaf && f.sel !== null) {
        ex.errors.push({ message: `Selections can't be made on scalars (field '${f.name}' returns ${name})` });
        ex.invalid = true;
      } else if (!leaf && f.sel === null) {
        ex.errors.push({ message: `Field must have selections (field '${f.name}' returns ${name} but has no selections)` });
        ex.invalid = true;
      } else if (!leaf && f.sel !== null && INTROSPECTION_TYPES[name] === undefined) {
        validate(ex, name, f.sel);
      }
    }
  }
}

function collectStatic(ex: Exec, typeName: string, sel: Selection[], out: Map<string, Field[]>): void {
  for (const s of sel) {
    if (s.kind === 'field') {
      const k = s.alias ?? s.name;
      out.set(k, [...(out.get(k) ?? []), s]);
    } else if (s.kind === 'inline') {
      if (s.on === null || s.on === typeName) collectStatic(ex, typeName, s.sel, out);
      else if (TYPES.has(s.on)) validate(ex, s.on, s.sel);
      else {
        ex.errors.push({ message: `No such type ${s.on}, so it can't be a fragment condition` });
        ex.invalid = true;
      }
    } else {
      const f = ex.doc.frags.get(s.name);
      if (f === undefined) {
        ex.errors.push({ message: `Fragment ${s.name} was used, but not defined` });
        ex.invalid = true;
      } else if (f.on === typeName) collectStatic(ex, typeName, f.sel, out);
      else if (TYPES.has(f.on)) validate(ex, f.on, f.sel);
    }
  }
}
