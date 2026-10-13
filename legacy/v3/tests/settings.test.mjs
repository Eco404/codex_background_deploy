import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { editSetting, parseSettings, run } from '../payload/settings.mjs';

const script = fileURLToPath(new URL('../payload/settings.mjs', import.meta.url));
function fixture(t, contents) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'codex-settings-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const settings = path.join(dir, 'settings.json');
  const snapshot = path.join(dir, 'state', 'settings.json');
  if (contents !== undefined) fs.writeFileSync(settings, contents, { mode: 0o640 });
  return { dir, settings, snapshot };
}

test('only the target value changes; URL, punctuation, comments, BOM and CRLF survive', () => {
  const original = '\ufeff{\r\n  "url": "https://example.test/a,}?q=/*x*/", // URL\r\n  "nested": {"chatgpt.cliExecutable": false},\r\n  "chatgpt.cliExecutable" /* key */: null, // target\r\n}\r\n';
  const updated = editSetting(original, true, '/path/with ",}/codex');
  assert.equal(updated, original.replace(': null', ': ' + JSON.stringify('/path/with ",}/codex')));
  assert.equal(parseSettings(updated).value, '/path/with ",}/codex');
});

test('removal remains valid for first, middle, last and trailing-comma members', () => {
  for (const text of [
    '{"chatgpt.cliExecutable":"x"}',
    '{"chatgpt.cliExecutable":"x",}',
    '{"chatgpt.cliExecutable":"x", // keep\n "a":1}',
    '{"a":1, "chatgpt.cliExecutable":"x", "b":2}',
    '{"a":1, // keep\n "chatgpt.cliExecutable":"x" // keep too\n}',
    '{"a":1, "chatgpt.cliExecutable":"x", // keep\n}',
  ]) {
    const result = editSetting(text, false, null);
    assert.equal(parseSettings(result).present, false);
    for (const comment of text.match(/\/\/[^\n]*/g) ?? []) assert.ok(result.includes(comment));
  }
});

test('insertion handles empty objects, comments and trailing comma without rewriting other bytes', () => {
  for (const text of ['{}', '{ // keep\n}', '{"a":1 // keep\n}', '{"a":1, // keep\n}']) {
    const result = editSetting(text, true, '/wrapper');
    assert.equal(parseSettings(result).value, '/wrapper');
    if (text.includes('// keep')) assert.ok(result.includes('// keep'));
  }
});

test('reject malformed JSONC, duplicate keys including escaped duplicates, and invalid target values', () => {
  for (const text of [
    '', '[]', '{} {}', '{"a":01}', '{"a":true false}', '{"a":1,,}',
    '{"a":1 "b":2}', '{/* unfinished', '{"a":"bad\nstring"}',
    '{"a":"\\q"}', '{"a":1,"a":2}', '{"a":1,"\\u0061":2}',
    '{"chatgpt.cliExecutable":true}', '{"chatgpt.cliExecutable":{}}',
    '{"chatgpt.cliExecutable":[]}', '{"a":[1,,2]}',
  ]) assert.throws(() => parseSettings(text), undefined, text);
});

test('inspect is read-only for missing files and emits JSON for null/string values', (t) => {
  const { settings, snapshot } = fixture(t);
  assert.equal(execFileSync(process.execPath, [script, 'inspect', settings, snapshot], { encoding: 'utf8' }), '__ABSENT__\n');
  assert.equal(fs.existsSync(settings), false);
  assert.equal(fs.existsSync(snapshot), false);
  fs.writeFileSync(settings, '{"chatgpt.cliExecutable":null}');
  assert.equal(run('inspect', settings, snapshot), 'null');
  fs.writeFileSync(settings, '{"chatgpt.cliExecutable":"https://a/,}"}');
  assert.equal(run('inspect', settings, snapshot), '"https://a/,}"');
});

test('repeated set keeps only original value; restore preserves unrelated edits and mode', (t) => {
  const original = '{"secret":"do-not-copy", "chatgpt.cliExecutable":null, "other":1}';
  const { settings, snapshot } = fixture(t, original);
  run('set', settings, snapshot, '/wrapper-v1');
  run('set', settings, snapshot, '/wrapper-v2');
  assert.deepEqual(JSON.parse(fs.readFileSync(snapshot, 'utf8')), {
    originalPresent: true, originalValue: null, installedValue: '/wrapper-v2',
  });
  assert.equal(fs.statSync(snapshot).mode & 0o777, 0o600);
  assert.equal(fs.statSync(settings).mode & 0o777, 0o640);
  fs.writeFileSync(settings, fs.readFileSync(settings, 'utf8').replace('"other":1', '"other":2'));
  run('restore', settings, snapshot);
  assert.equal(fs.readFileSync(settings, 'utf8'), original.replace('"other":1', '"other":2'));
  assert.equal(fs.existsSync(snapshot), false);
  run('set', settings, snapshot, '/wrapper-v3');
  run('restore', settings, snapshot);
  assert.equal(parseSettings(fs.readFileSync(settings, 'utf8')).value, null);
});

test('originally absent key or file restores absence', (t) => {
  for (const original of [undefined, '{"existing":1, // retained\n}']) {
    const { settings, snapshot } = fixture(t, original);
    run('set', settings, snapshot, '/wrapper');
    run('restore', settings, snapshot);
    assert.equal(parseSettings(fs.readFileSync(settings, 'utf8')).present, false);
    if (original) assert.ok(fs.readFileSync(settings, 'utf8').includes('"existing":1, // retained'));
  }
});

test('user changes conflict on set and restore; missing settings retains snapshot', (t) => {
  const { settings, snapshot } = fixture(t, '{}');
  run('set', settings, snapshot, '/wrapper');
  const saved = fs.readFileSync(snapshot, 'utf8');
  fs.writeFileSync(settings, '{"chatgpt.cliExecutable":"/user-change"}');
  assert.throws(() => run('restore', settings, snapshot), /conflict/);
  assert.throws(() => run('set', settings, snapshot, '/next'), /conflict/);
  assert.equal(fs.readFileSync(snapshot, 'utf8'), saved);
  assert.equal(parseSettings(fs.readFileSync(settings, 'utf8')).value, '/user-change');
  fs.unlinkSync(settings);
  assert.throws(() => run('restore', settings, snapshot), /ENOENT/);
  assert.equal(fs.readFileSync(snapshot, 'utf8'), saved);
});

test('rename failure preserves settings and rolls ownership snapshot back without temp residue', (t) => {
  const { dir, settings, snapshot } = fixture(t, '{"chatgpt.cliExecutable":"/original"}');
  const realRename = fs.renameSync;
  const failSettingsRename = () => {
    fs.renameSync = (from, to) => {
      if (to === settings) throw Object.assign(new Error('simulated rename failure'), { code: 'EIO' });
      return realRename(from, to);
    };
  };
  t.after(() => { fs.renameSync = realRename; });
  failSettingsRename();
  assert.throws(() => run('set', settings, snapshot, '/first'), /simulated/);
  assert.equal(fs.existsSync(snapshot), false);
  assert.equal(parseSettings(fs.readFileSync(settings, 'utf8')).value, '/original');
  fs.renameSync = realRename;
  run('set', settings, snapshot, '/first');
  const saved = fs.readFileSync(snapshot, 'utf8');
  failSettingsRename();
  assert.throws(() => run('set', settings, snapshot, '/second'), /simulated/);
  assert.equal(fs.readFileSync(snapshot, 'utf8'), saved);
  assert.equal(parseSettings(fs.readFileSync(settings, 'utf8')).value, '/first');
  assert.throws(() => run('restore', settings, snapshot), /simulated/);
  assert.equal(fs.readFileSync(snapshot, 'utf8'), saved);
  assert.equal(fs.readdirSync(dir).some((name) => name.includes('.tmp-')), false);
  assert.equal(fs.readdirSync(path.dirname(snapshot)).some((name) => name.includes('.tmp-')), false);
});
