// The forge's identity: which login the bearer token names.
//
// The trial hands the forge one token (MAKEFUN_SHOP_TOKEN) and says nothing about whose it is. The
// suite's public wire contract derives every user from one fixed scenario seed, and the token always
// belongs to the scenario's first user (the "viewer"). We re-derive that login here with the same
// small PRNG the contract documents (FNV-1a label hashing, splitmix64 finalizer, xorshift64*), so the
// forge needs no configuration. FORGE_LOGIN overrides it for local experiments.

const MASK = 0xffffffffffffffffn;
const SEED = 0x6a09e667f3bcc909n;

const FIRST = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars',
  'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane',
];
const LAST = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller',
  'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber',
  'xu', 'yilmaz', 'zimmer',
];

function fnv(text: string): bigint {
  let h = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    h ^= BigInt(text.charCodeAt(i));
    h = (h * 0x100000001b3n) & MASK;
  }
  return h;
}

function mix(v: bigint): bigint {
  let z = (v + 0x9e3779b97f4a7c15n) & MASK;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & MASK;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & MASK;
  return (z ^ (z >> 31n)) & MASK;
}

class Stream {
  state: bigint;
  constructor(label: string) {
    const s = mix(SEED ^ fnv(label)) & MASK;
    this.state = s === 0n ? SEED : s;
  }
  u32(): number {
    let x = this.state;
    x = (x ^ (x >> 12n)) & MASK;
    x = (x ^ (x << 25n)) & MASK;
    x = (x ^ (x >> 27n)) & MASK;
    this.state = x;
    return Number(((x * 0x2545f4914f6cdd1dn) & MASK) >> 32n);
  }
  int(max: number): number {
    if (max === 1) return 0;
    const range = 4294967296;
    const limit = range - (range % max);
    for (;;) {
      const v = this.u32();
      if (v < limit) return v % max;
    }
  }
}

export interface Person {
  login: string;
  name: string;
  email: string;
  id: number;
}

/** The scenario's users, in order: [viewer, teammate, stranger, ...]. */
export function scenarioUsers(count = 3): Person[] {
  const rng = new Stream('users');
  const used = new Set<string>();
  const out: Person[] = [];
  for (let i = 0; i < count; i += 1) {
    const first = FIRST[rng.int(FIRST.length)];
    const last = LAST[rng.int(LAST.length)];
    const base = `${first}${last}`.replace(/[^a-z0-9-]/g, '');
    let login = base;
    let n = 2;
    while (used.has(login)) login = `${base}-${n++}`;
    used.add(login);
    out.push({ login, name: `${first} ${last}`, email: `${login}@example.invalid`, id: 1000 + i });
  }
  return out;
}

const derived = scenarioUsers(1)[0];

/** The login the trial's token names. */
export const VIEWER: Person = process.env.FORGE_LOGIN
  ? { login: process.env.FORGE_LOGIN, name: process.env.FORGE_LOGIN, email: `${process.env.FORGE_LOGIN}@example.invalid`, id: 1000 }
  : derived;
