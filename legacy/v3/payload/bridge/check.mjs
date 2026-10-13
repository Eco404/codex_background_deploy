import { pathToFileURL } from 'node:url';
import { connectToDaemon } from './bridge.mjs';

export async function checkDaemon(socketPath, { timeoutMs = 5000 } = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  let ws;
  try {
    ws = await connectToDaemon(socketPath, { timeoutMs, signal: controller.signal });
    return await new Promise((resolve, reject) => {
      const abort = () => reject(new Error('Initialize timed out'));
      controller.signal.addEventListener('abort', abort, { once: true });
      ws.once('error', reject);
      ws.once('close', () => reject(new Error('App-server closed before initialize')));
      ws.on('message', (data, binary) => {
        if (binary) return;
        let message;
        try { message = JSON.parse(data.toString('utf8')); } catch { reject(new Error('Invalid JSON reply')); return; }
        if (message.id !== 1) return;
        if (message.error || !message.result) { reject(new Error('Initialize rejected')); return; }
        ws.send(JSON.stringify({ method: 'initialized' }), error => {
          if (error) reject(error); else resolve({ ok: true });
        });
      });
      ws.send(JSON.stringify({ id: 1, method: 'initialize', params: {
        clientInfo: { name: 'codex-background-health', version: '1.0.0' },
      } }), error => { if (error) reject(error); });
      ws.resume();
      if (controller.signal.aborted) abort();
    });
  } finally {
    clearTimeout(timer);
    ws?.terminate();
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    await checkDaemon(process.argv[2] || process.env.CB_SOCKET, {
      timeoutMs: Number(process.env.CB_CONNECT_TIMEOUT_MS || 5000),
    });
    process.stdout.write('app-server initialize: OK\n');
  } catch {
    process.stderr.write('app-server initialize: FAILED (socket, handshake or initialize)\n');
    process.exitCode = 1;
  }
}
