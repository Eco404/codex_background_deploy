import test from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { createServer as httpServer } from 'node:http';
import { createServer as netServer } from 'node:net';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { PassThrough, Writable } from 'node:stream';
import { spawnSync } from 'node:child_process';
import { connectToDaemon, runBridge, metadataLog } from '../../payload/bridge/bridge.mjs';
import { checkDaemon } from '../../payload/bridge/check.mjs';
const require = createRequire(import.meta.url);
const { WebSocketServer } = require('ws');
const payload = fileURLToPath(new URL('../../payload/bridge/', import.meta.url));
const bounded = promise => Promise.race([promise, new Promise((_, reject) => {
  const timer = setTimeout(() => reject(new Error('test operation timed out')), 2500); timer.unref();
})]);

async function fixture(t, behavior = socket => socket.on('message', data => {
  const msg = JSON.parse(data);
  if (msg.method === 'initialize') socket.send(JSON.stringify({ id: msg.id, result: { userAgent: 'test' } }));
})) {
  const dir = mkdtempSync(join(tmpdir(), 'cb-bridge-'));
  const socketPath = join(dir, 'has space:colon.sock');
  const server = httpServer();
  const wss = new WebSocketServer({ server });
  wss.on('connection', behavior);
  await new Promise(resolve => server.listen(socketPath, resolve));
  t.after(async () => {
    for (const c of wss.clients) c.terminate();
    await new Promise(resolve => wss.close(resolve));
    await new Promise(resolve => server.close(resolve));
    rmSync(dir, { recursive: true, force: true });
  });
  return { dir, socketPath, wss, server };
}

test('health performs initialize over Unix WebSocket and cleans up', async t => {
  const { socketPath } = await fixture(t);
  assert.deepEqual(await bounded(checkDaemon(socketPath, { timeoutMs: 500 })), { ok: true });
});

test('single incomplete HTTP handshake respects overall deadline', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'cb-stalled-')); const socketPath = join(dir, 'server.sock');
  const clients = new Set();
  const server = netServer(socket => { clients.add(socket); socket.on('close', () => clients.delete(socket)); });
  await new Promise(resolve => server.listen(socketPath, resolve));
  t.after(async () => { for (const c of clients) c.destroy(); await new Promise(r => server.close(r)); rmSync(dir, { recursive: true, force: true }); });
  const started = Date.now();
  await assert.rejects(connectToDaemon(socketPath, { timeoutMs: 120 }));
  assert.ok(Date.now() - started < 900);
});

test('health initialize timeout applies after successful HTTP upgrade', async t => {
  const { socketPath } = await fixture(t, () => {});
  const started = Date.now();
  await assert.rejects(checkDaemon(socketPath, { timeoutMs: 120 }));
  assert.ok(Date.now() - started < 900);
});

test('bridge preserves JSONL protocol and exits on stdin EOF', async t => {
  const { socketPath, wss } = await fixture(t);
  const input = new PassThrough(); const output = new PassThrough(); let text = '';
  const got = new Promise(resolve => output.on('data', data => { text += data; if (text.includes('\n')) resolve(); }));
  const result = runBridge({ input, output, socketPath });
  input.write('{"id":1,"method":"initialize","params":{}}\n');
  await bounded(got);
  assert.equal(JSON.parse(text).id, 1);
  input.end();
  assert.equal((await bounded(result)).ok, true);
  assert.equal(input.listenerCount('data'), 0);
  await new Promise(resolve => setImmediate(resolve));
  assert.ok([...wss.clients].every(client => client.readyState !== 1));
});

test('unexpected WebSocket close terminates bridge with stdin still open', async t => {
  const { socketPath } = await fixture(t, socket => setTimeout(() => socket.close(), 20));
  const input = new PassThrough(); const output = new PassThrough();
  assert.equal((await bounded(runBridge({ input, output, socketPath }))).ok, false);
  assert.equal(input.listenerCount('data'), 0);
});

test('output EPIPE and input errors clean up without unhandled errors', async t => {
  const { socketPath } = await fixture(t, socket => socket.send('{"secret":"do not log"}'));
  const input = new PassThrough();
  const output = new Writable({ write(_data, _enc, callback) { callback(Object.assign(new Error('closed'), { code: 'EPIPE' })); } });
  assert.equal((await bounded(runBridge({ input, output, socketPath }))).code, 'EPIPE');
  const nextInput = new PassThrough();
  const nextOutput = new PassThrough();
  const controller = new AbortController();
  const waiting = runBridge({ input: nextInput, output: nextOutput, socketPath, signal: controller.signal,
    onEvent(event) { if (event === 'connected') nextInput.destroy(Object.assign(new Error('bad input'), { code: 'EIO' })); } });
  assert.equal((await bounded(waiting)).code, 'EIO');
});

test('abort during stalled handshake promptly releases socket', async t => {
  const { socketPath } = await fixture(t);
  const controller = new AbortController(); controller.abort();
  await assert.rejects(connectToDaemon(socketPath, { signal: controller.signal }), { code: 'ABORT_ERR' });
});

test('logs rotate and never include message bodies', t => {
  const dir = mkdtempSync(join(tmpdir(), 'cb-log-')); t.after(() => rmSync(dir, { recursive: true, force: true }));
  const log = join(dir, 'bridge.log'); writeFileSync(log, 'x'.repeat(270000));
  metadataLog(log, 'failed', { sent: 1, received: 2, code: 'EPIPE', message: 'user secret' });
  assert.equal(readFileSync(`${log}.1`, 'utf8').length, 270000);
  assert.ok(!readFileSync(log, 'utf8').includes('user secret'));
});

function nodeFixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'cb-node-')); t.after(() => rmSync(dir, { recursive: true, force: true }));
  const nvm = join(dir, '.nvm');
  mkdirSync(join(nvm, 'alias'), { recursive: true }); writeFileSync(join(nvm, 'alias/default'), 'missing-version');
  writeFileSync(join(nvm, 'nvm.sh'), 'echo MUST_NOT_SOURCE\nreturn 99\n');
  for (const version of ['v18.20.0', 'v22.1.0', 'v99.0.0']) {
    const bin = join(nvm, 'versions/node', version, 'bin'); mkdirSync(bin, { recursive: true });
    writeFileSync(join(bin, 'node'), version === 'v99.0.0' ? '#!/bin/bash\nexit 1\n' : '#!/bin/bash\nprintf "%s\\n" 22\n', { mode: 0o700 });
  }
  return { dir, nvm };
}
function resolveNode(env) {
  return spawnSync('/bin/bash', ['--noprofile', '--norc', '-c', 'source "$1"; cb_resolve_node', 'bash', join(payload, 'resolve-node.sh')], {
    env, encoding: 'utf8', timeout: 3000,
  });
}

test('Node lookup ignores nvm aliases and failed newest executable', t => {
  const { dir, nvm } = nodeFixture(t);
  const bin = join(dir, 'minimal'); mkdirSync(bin);
  // Only sort is available; fake node versions run /bin/bash directly.
  writeFileSync(join(bin, 'sort'), '#!/bin/bash\nexec /usr/bin/sort "$@"\n', { mode: 0o700 });
  const r = resolveNode({ HOME: dir, PATH: bin, NVM_DIR: nvm, CB_NODE_PIN: '/missing/node' });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), join(nvm, 'versions/node/v22.1.0/bin/node'));
});

test('explicit invalid Node fails rather than silently falling back', t => {
  const { dir, nvm } = nodeFixture(t);
  for (const CODEX_NODE of ['/does/not/exist', '', 'node']) {
    const r = resolveNode({ HOME: dir, PATH: '/usr/bin:/bin', NVM_DIR: nvm, CODEX_NODE });
    assert.equal(r.status, 1); assert.equal(r.stdout, ''); assert.match(r.stderr, /CODEX_NODE/);
  }
});

test('wrapper routes real CLI management and help without resolving Node', t => {
  const dir = mkdtempSync(join(tmpdir(), 'cb-wrapper-')); t.after(() => rmSync(dir, { recursive: true, force: true }));
  const fakeCodex = join(dir, 'real-codex');
  writeFileSync(fakeCodex, '#!/bin/bash\nprintf "real:%s\\n" "$*"\n', { mode: 0o700 });
  writeFileSync(join(dir, 'config.sh'), `CB_CODEX=${JSON.stringify(fakeCodex)}\nCB_CODEX_HOME=${JSON.stringify(join(dir, '.codex'))}\nCB_SOCKET=${JSON.stringify(join(dir, 's.sock'))}\n`);
  for (const args of [['--version'], ['app-server', '--help'], ['app-server', 'daemon', 'start'],
    ['app-server', 'proxy'], ['--help', 'app-server'], ['-c', 'app-server', 'exec'], ['exec', 'app-server']]) {
    const result = spawnSync('/bin/bash', [join(payload, 'wrapper.sh'), ...args], {
      env: { HOME: dir, PATH: '/usr/bin:/bin', CB_ROOT: dir, CODEX_NODE: '/missing/node' }, encoding: 'utf8', timeout: 3000,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), `real:${args.join(' ')}`);
  }
  const result = spawnSync('/bin/bash', [join(payload, 'wrapper.sh'), 'app-server', '--stdio'], {
    env: { HOME: dir, PATH: '/usr/bin:/bin', CB_ROOT: dir, CODEX_NODE: '/missing/node' }, encoding: 'utf8', timeout: 3000,
  });
  assert.equal(result.status, 1); assert.match(result.stderr, /CODEX_NODE/); assert.equal(result.stdout, '');
});
