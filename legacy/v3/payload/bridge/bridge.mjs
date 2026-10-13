import { createRequire } from 'node:module';
import { createConnection } from 'node:net';
import { createInterface } from 'node:readline';
import { appendFileSync, renameSync, statSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
const require = createRequire(import.meta.url);
const WebSocket = require('ws');

const abortError = () => Object.assign(new Error('Connection cancelled'), { code: 'ABORT_ERR' });
const timeoutError = () => Object.assign(new Error('App-server connection timed out'), { code: 'ETIMEDOUT' });

function connectOnce(socketPath, timeoutMs, signal) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(abortError()); return; }
    // socketPath is supplied explicitly: spaces and colons in HOME are not URL syntax.
    const ws = new WebSocket('ws://localhost/rpc', {
      createConnection: () => createConnection({ path: socketPath }),
      perMessageDeflate: false, handshakeTimeout: timeoutMs,
    });
    let settled = false;
    const timer = setTimeout(() => finish(timeoutError()), timeoutMs);
    const onAbort = () => finish(abortError());
    const onError = error => finish(error);
    const onClose = () => finish(new Error('Connection closed before handshake'));
    function finish(error) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener('abort', onAbort);
      ws.off('open', onOpen);
      ws.off('close', onClose);
      ws.off('error', onError);
      // ws may emit an asynchronous error after terminate() during HTTP upgrade.
      ws.on('error', () => {});
      if (error) { ws.terminate(); reject(error); } else { resolve(ws); }
    }
    // Pause until the caller has attached protocol listeners (upgrade may include frames).
    const onOpen = () => { ws.pause(); finish(); };
    signal?.addEventListener('abort', onAbort, { once: true });
    ws.once('open', onOpen);
    ws.once('error', onError);
    ws.once('close', onClose);
  });
}

export async function connectToDaemon(socketPath, { timeoutMs = 5000, retryMs = 100, signal } = {}) {
  if (!socketPath || !Number.isFinite(timeoutMs) || timeoutMs <= 0) throw new Error('Invalid socket or timeout');
  const deadline = Date.now() + timeoutMs;
  let lastError;
  while (Date.now() < deadline) {
    if (signal?.aborted) throw abortError();
    try { return await connectOnce(socketPath, Math.max(1, deadline - Date.now()), signal); }
    catch (error) { lastError = error; }
    if (signal?.aborted) throw abortError();
    const delay = Math.min(retryMs, deadline - Date.now());
    if (delay > 0) await new Promise(resolve => {
      const done = () => { clearTimeout(timer); signal?.removeEventListener('abort', done); resolve(); };
      const timer = setTimeout(done, delay);
      signal?.addEventListener('abort', done, { once: true });
    });
  }
  throw lastError ?? timeoutError();
}

export function metadataLog(file, event, details = {}) {
  if (!file) return;
  // Log only our own event names, counts and error codes, never protocol payloads.
  const record = { time: new Date().toISOString(), event };
  for (const key of ['sent', 'received', 'code']) {
    if (typeof details[key] === 'number' || (key === 'code' && /^[A-Z0-9_]+$/.test(String(details[key])))) record[key] = details[key];
  }
  try {
    try { if (statSync(file).size > 256 * 1024) renameSync(file, `${file}.1`); } catch {}
    appendFileSync(file, `${JSON.stringify(record)}\n`, { mode: 0o600 });
  } catch { /* Diagnostics must never break the protocol stream. */ }
}

export async function runBridge({ input, output, socketPath, timeoutMs = 5000, signal, onEvent = () => {} }) {
  const ws = await connectToDaemon(socketPath, { timeoutMs, signal });
  return new Promise(resolve => {
    let done = false, closing = false, closeTimer, sent = 0, received = 0;
    const lines = createInterface({ input, crlfDelay: Infinity });
    const emit = (event, details = {}) => { try { onEvent(event, details); } catch {} };
    const finish = (ok, code) => {
      if (done) return;
      done = true;
      clearTimeout(closeTimer);
      signal?.removeEventListener('abort', onAbort);
      lines.off('line', onLine);
      lines.off('close', onInputClose);
      lines.off('error', onInputError);
      lines.close();
      input.off('error', onInputError);
      input.pause();
      output.off('drain', onDrain);
      // Keep an inert error listener until process exit: pending writes may fail after EOF.
      output.off('error', onOutputError);
      output.on('error', () => {});
      ws.off('message', onMessage);
      ws.off('close', onClose);
      ws.off('error', onWsError);
      ws.terminate();
      emit(ok ? 'disconnected' : 'failed', { sent, received, code });
      resolve({ ok, sent, received, code });
    };
    const requestClose = () => {
      if (done || closing) return;
      closing = true;
      lines.pause();
      // Bound the peer's close handshake; no client disconnect can leave a bridge behind.
      closeTimer = setTimeout(() => finish(true, 1000), 500);
      try { ws.close(1000); } catch { finish(true, 1000); }
    };
    const onAbort = requestClose;
    const onInputClose = requestClose;
    const onInputError = error => finish(false, error.code || 'INPUT_ERROR');
    const onOutputError = error => finish(false, error.code || 'OUTPUT_ERROR');
    const onWsError = error => finish(false, error.code || 'WEBSOCKET_ERROR');
    const onClose = code => finish(closing, code);
    const onDrain = () => { if (!done && ws.readyState === WebSocket.OPEN) ws.resume(); };
    const onMessage = (data, binary) => {
      if (done || closing) return;
      if (binary) { finish(false, 'BINARY_FRAME'); return; }
      received++;
      try { if (!output.write(`${data.toString('utf8')}\n`)) ws.pause(); }
      catch (error) { onOutputError(error); }
    };
    const onLine = line => {
      if (done || closing || !line.trim()) return;
      if (Buffer.byteLength(line) > 64 * 1024 * 1024) { finish(false, 'INPUT_TOO_LARGE'); return; }
      sent++;
      try {
        ws.send(line, error => {
          if (error) { onWsError(error); return; }
          if (!done && !closing && ws.bufferedAmount < 1024 * 1024) lines.resume();
        });
        if (ws.bufferedAmount >= 1024 * 1024) lines.pause();
      } catch (error) { onWsError(error); }
    };
    ws.on('message', onMessage);
    ws.once('close', onClose);
    ws.once('error', onWsError);
    output.on('drain', onDrain);
    output.on('error', onOutputError);
    input.on('error', onInputError);
    lines.on('error', onInputError);
    lines.on('line', onLine);
    lines.once('close', onInputClose);
    signal?.addEventListener('abort', onAbort, { once: true });
    emit('connected');
    if (!done) ws.resume();
    if (signal?.aborted || input.readableEnded || input.destroyed) requestClose();
  });
}

async function main() {
  const controller = new AbortController();
  const abort = () => controller.abort();
  process.once('SIGTERM', abort);
  process.once('SIGINT', abort);
  try {
    const result = await runBridge({ input: process.stdin, output: process.stdout,
      socketPath: process.env.CB_SOCKET, timeoutMs: Number(process.env.CB_CONNECT_TIMEOUT_MS || 5000),
      signal: controller.signal, onEvent: (event, details) => metadataLog(process.env.CB_LOG, event, details) });
    process.exitCode = result.ok ? 0 : 1;
  } catch (error) {
    metadataLog(process.env.CB_LOG, 'connection_failed', { code: error.code || 'CONNECT_ERROR' });
    process.stderr.write('codex-background: cannot connect to app-server; run deploy.sh status, doctor or start.\n');
    process.exitCode = 1;
  } finally {
    process.removeListener('SIGTERM', abort);
    process.removeListener('SIGINT', abort);
    process.stdin.pause();
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
