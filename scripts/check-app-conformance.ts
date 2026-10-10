// Run with Bun and an existing app checkout. No app files or dependencies change.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { Database, type SQLQueryBindings } from 'bun:sqlite';

const [appRoot, baseURL = 'http://127.0.0.1:8089'] = process.argv.slice(2);
if (!appRoot) throw new Error('Usage: bun scripts\\check-app-conformance.ts <app checkout> [fixture URL]');
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const appModule = (name: string) =>
  import(pathToFileURL(join(resolve(appRoot), 'src', 'features', 'map', 'tiles', name)).href);
const { TileStreamDecoder, STREAM_MAX_BYTES, STREAM3_MAX_BYTES } = await appModule('bundle-stream.ts');
const { StreamingBundleSource } = await appModule('streaming-bundle-source.ts');
const { SqliteBundleResumeStore } = await appModule('bundle-resume-store.ts');
type Request = { anchorZoom: number; anchorX: number; anchorY: number; tileZoom: number };
type Entry = { tile: { z: number; x: number; y: number }; bytes: Uint8Array | null };
type Stage = { tileZoom: number; part: 'full' | 'structure' | 'labels' };
const request: Request = { anchorZoom: 10, anchorX: 164, anchorY: 357, tileZoom: 11 };
const hash = async (raw: Uint8Array) => new Uint8Array(createHash('sha256').update(raw).digest());
const golden2 = new Uint8Array(await readFile(join(root, 'testdata', 'scb2-z11-empty.scb2')));
const golden = new Uint8Array(await readFile(join(root, 'testdata', 'scb3-z11-empty.scb3')));
const golden14 = new Uint8Array(await readFile(join(root, 'testdata', 'scb3-z14-empty.scb3')));
assert.equal(golden2.length, 107);
assert.equal(golden.length, 96);
assert.equal(STREAM_MAX_BYTES, 20 + 2 * (65 * 1024 * 1024 + 40));
assert.equal(STREAM3_MAX_BYTES, 20 + 3 * (64 * 1024 * 1024 + 40));

function validate(stage: Request, entries: readonly Entry[]) {
  const side = 2 ** (stage.tileZoom - 10);
  assert.equal(entries.length, side * side);
  entries.forEach((entry, i) => {
    assert.equal(entry.bytes, null);
    assert.deepEqual(entry.tile, {
      z: stage.tileZoom,
      x: stage.anchorX * side + (i % side),
      y: stage.anchorY * side + Math.floor(i / side),
    });
  });
}
// The v2 decoder stays the fallback for servers without v3.
for (const fragmentSize of [1, 7, 20, 40, 107]) {
  const stages: number[] = [];
  const decoder = new TileStreamDecoder(request, hash, async (stage: Request, entries: Entry[]) => {
    validate(stage, entries);
    stages.push(stage.tileZoom);
  }, 2);
  for (let i = 0; i < golden2.length; i += fragmentSize) {
    await decoder.push(golden2.subarray(i, i + fragmentSize));
  }
  decoder.finish();
  assert.deepEqual(stages, [11]);
}
for (const [bytes, tileZoom, want] of [
  [golden, 11, ['11:full']],
  [golden14, 14, ['13:full', '14:structure', '14:labels']],
] as const) {
  for (const fragmentSize of [1, 7, 20, 40, bytes.length]) {
    const stages: string[] = [];
    const decoder = new TileStreamDecoder({ ...request, tileZoom }, hash, async (stage: Request, entries: Entry[], part: Stage) => {
      validate(stage, entries);
      assert.equal(part.tileZoom, stage.tileZoom);
      stages.push(`${part.tileZoom}:${part.part}`);
    }, 3);
    for (let i = 0; i < bytes.length; i += fragmentSize) {
      await decoder.push(bytes.subarray(i, i + fragmentSize));
    }
    decoder.finish();
    assert.deepEqual(stages, want);
  }
}

const sourceURL = `${baseURL.replace(/\/+$/, '')}/planet`;
const url = `${sourceURL}/bundle/v3/164/357/11`;
const live = await fetch(url);
assert.equal(live.headers.get('content-type'), 'application/vnd.streetcryptid.tile-stream3');
assert.equal(live.status, 200);
assert.equal(live.headers.get('content-encoding'), null);
assert.equal(live.headers.get('cache-control'), 'public, max-age=86400, no-transform');
assert.deepEqual(new Uint8Array(await live.arrayBuffer()), golden);
const etag = live.headers.get('etag');
assert.ok(etag && /^"[^"]+"$/.test(etag));

const temporary = await mkdtemp(join(tmpdir(), 'scb3-app-conformance-'));
try {
  // Use the app's actual SQLite store with a host SQLite adapter, reopening the
  // database between prefix persistence and download to model a process restart.
  const dbPath = join(temporary, 'resume.sqlite');
  let db = new Database(dbPath, { create: true });
  db.exec(`CREATE TABLE bundle_downloads (
    key TEXT NOT NULL, etag TEXT NOT NULL, offset INTEGER NOT NULL,
    bytes BLOB NOT NULL, at INTEGER NOT NULL, PRIMARY KEY (key, offset)
  )`);
  const adapter = {
    async execAsync(sql: string) { db.exec(sql); },
    async runAsync(sql: string, ...params: SQLQueryBindings[]) { return db.query(sql).run(...params); },
    async getFirstAsync<T>(sql: string, ...params: SQLQueryBindings[]): Promise<T | null> {
      return db.query<T, SQLQueryBindings[]>(sql).get(...params);
    },
  };
  try {
    for (const offset of [0, 1, 19, 20, 39, 60, 95, 96]) {
      let store = new SqliteBundleResumeStore(adapter);
      if (offset) await store.append(url, etag, 0, golden.slice(0, offset));
      db.close(true);
      db = new Database(dbPath);
      store = new SqliteBundleResumeStore(adapter);
      const statuses: number[] = [];
      const fetcher = async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
        const response = await fetch(input, init);
        statuses.push(response.status);
        if (offset) {
          assert.equal(new Headers(init?.headers).get('range'), `bytes=${offset}-`);
          assert.equal(new Headers(init?.headers).get('if-range'), etag);
        }
        return response;
      };
      const source = new StreamingBundleSource(sourceURL, async () => store, hash, fetcher);
      validate(request, await source.getBundle(request));
      assert.deepEqual(statuses, [offset === 96 ? 416 : offset ? 206 : 200]);
      assert.equal(await store.state(url), null);
    }
    const store = new SqliteBundleResumeStore(adapter);
    await store.append(url, '"obsolete-dataset"', 0, golden.slice(0, 60));
    const source = new StreamingBundleSource(sourceURL, async () => store, hash);
    validate(request, await source.getBundle(request));
    assert.equal(await store.state(url), null);
    const stages: string[] = [];
    const detail = { ...request, tileZoom: 14 };
    validate(detail, await source.getBundle(detail, async (stage: Request, entries: Entry[], part: Stage) => {
      validate(stage, entries);
      stages.push(`${part.tileZoom}:${part.part}`);
    }));
    assert.deepEqual(stages, ['13:full', '14:structure', '14:labels']);
  } finally {
    db.close(true);
  }
} finally {
  await rm(temporary, { recursive: true });
}
console.log('PASS: Go goldens (SCB2 107 bytes, SCB3 z11 96 bytes + z14), fragmented app parsers, live v3 handler, reopened SQLite prefixes, 206/416, changed ETag, z13 / z14 structure / z14 labels stages.');
