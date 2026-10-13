#!/usr/bin/env node
// Edit one top-level JSONC setting without copying unrelated settings or secrets.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const KEY = 'chatgpt.cliExecutable';
const own = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

export function parseSettings(text) {
  const tokens = [];
  let offset = text.charCodeAt(0) === 0xfeff ? 1 : 0;
  const invalid = (at = offset) => { throw new Error(`Invalid JSONC at offset ${at}`); };
  while (offset < text.length) {
    if (/[\t\n\r ]/.test(text[offset])) { offset++; continue; }
    if (text.startsWith('//', offset)) {
      const end = text.indexOf('\n', offset + 2);
      offset = end < 0 ? text.length : end;
      continue;
    }
    if (text.startsWith('/*', offset)) {
      const end = text.indexOf('*/', offset + 2);
      if (end < 0) invalid();
      offset = end + 2;
      continue;
    }
    const start = offset;
    const char = text[offset];
    if ('{}[]:,'.includes(char)) {
      tokens.push({ type: char, start, end: ++offset });
    } else if (char === '"') {
      offset++;
      let closed = false;
      while (offset < text.length) {
        const next = text[offset++];
        if (next === '\\') { offset++; continue; }
        if (next === '"') { closed = true; break; }
      }
      if (!closed) invalid(start);
      let value;
      try { value = JSON.parse(text.slice(start, offset)); } catch { invalid(start); }
      tokens.push({ type: 'string', value, start, end: offset });
    } else {
      const match = /^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(text.slice(offset));
      if (!match) invalid();
      offset += match[0].length;
      tokens.push({ type: 'literal', value: JSON.parse(match[0]), start, end: offset });
    }
  }

  let cursor = 0;
  const take = (type) => {
    const token = tokens[cursor];
    if (!token || token.type !== type) invalid(token?.start ?? text.length);
    cursor++;
    return token;
  };
  function value(depth = 0) {
    if (depth > 256) throw new Error('JSONC nesting exceeds 256 levels');
    const first = tokens[cursor];
    if (!first) invalid(text.length);
    if (first.type === 'string' || first.type === 'literal') {
      cursor++;
      return { start: first.start, end: first.end, value: first.value };
    }
    if (first.type !== '{' && first.type !== '[') invalid(first.start);
    cursor++;
    const object = first.type === '{';
    const close = object ? '}' : ']';
    const members = [];
    const keys = new Set();
    while (tokens[cursor]?.type !== close) {
      let key;
      if (object) {
        key = take('string');
        if (keys.has(key.value)) throw new Error(`Duplicate JSONC key at offset ${key.start}`);
        keys.add(key.value);
        take(':');
      }
      const child = value(depth + 1);
      const member = { key: key?.value, start: key?.start ?? child.start, ...child };
      // A member starts at its key, whereas its value starts after the colon.
      member.valueStart = child.start;
      member.start = key?.start ?? child.start;
      members.push(member);
      if (tokens[cursor]?.type !== ',') break;
      member.comma = take(',');
    }
    const last = take(close);
    return { start: first.start, end: last.end, close: last.start, members, object };
  }
  const root = value();
  if (!root.object || cursor !== tokens.length) invalid(tokens[cursor]?.start ?? root.start);
  const index = root.members.findIndex((member) => member.key === KEY);
  const member = root.members[index];
  if (member && typeof member.value !== 'string' && member.value !== null) {
    throw new Error(`${KEY} must be a string or null`);
  }
  return { root, index, member, present: !!member, value: member?.value };
}

export function editSetting(text, present, value) {
  const parsed = parseSettings(text);
  if (present && value !== null && typeof value !== 'string') throw new Error('Setting must be a string or null');
  if (parsed.member) {
    const member = parsed.member;
    if (present) return text.slice(0, member.valueStart) + JSON.stringify(value) + text.slice(member.end);
    if (member.comma) return text.slice(0, member.start) + text.slice(member.comma.end);
    const preceding = parsed.root.members[parsed.index - 1]?.comma;
    if (preceding) {
      return text.slice(0, preceding.start) + text.slice(preceding.end, member.start) + text.slice(member.end);
    }
    return text.slice(0, member.start) + text.slice(member.end);
  }
  if (!present) return text;
  const last = parsed.root.members.at(-1);
  let before = text.slice(0, parsed.root.close);
  if (last && !last.comma) before = before.slice(0, last.end) + ',' + before.slice(last.end);
  const addition = `\n  ${JSON.stringify(KEY)}: ${JSON.stringify(value)}${last?.comma ? ',' : ''}\n`;
  return before + addition + text.slice(parsed.root.close);
}

function readRegular(file, allowMissing = false) {
  let stat;
  try { stat = fs.lstatSync(file); } catch (error) {
    if (allowMissing && error.code === 'ENOENT') return null;
    throw error;
  }
  if (!stat.isFile()) throw new Error(`Expected a regular file: ${file}`);
  return { text: fs.readFileSync(file, 'utf8'), mode: stat.mode & 0o777 };
}

function atomicWrite(file, text, mode) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const temporary = `${file}.tmp-${process.pid}-${crypto.randomBytes(6).toString('hex')}`;
  let fd;
  try {
    fd = fs.openSync(temporary, 'wx', mode);
    fs.writeFileSync(fd, text, 'utf8');
    fs.fchmodSync(fd, mode);
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fd = undefined;
    fs.renameSync(temporary, file);
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
    try { fs.unlinkSync(temporary); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
}

function decodeSnapshot(file) {
  const saved = readRegular(file, true);
  if (!saved) return null;
  const snapshot = JSON.parse(saved.text);
  if (!snapshot || typeof snapshot.originalPresent !== 'boolean'
      || typeof snapshot.installedValue !== 'string'
      || !own(snapshot, 'originalValue')
      || (snapshot.originalValue !== null && typeof snapshot.originalValue !== 'string')
      || Object.keys(snapshot).some((key) => !['originalPresent', 'originalValue', 'installedValue'].includes(key))) {
    throw new Error(`Invalid settings snapshot: ${file}`);
  }
  return snapshot;
}

export function run(command, settingsPath, snapshotPath, wrapperPath) {
  if (!['inspect', 'set', 'restore'].includes(command) || !settingsPath || !snapshotPath) {
    throw new Error('Usage: settings.mjs <inspect|set|restore> <settings-path> <snapshot-path> [wrapper-path]');
  }
  if (path.resolve(settingsPath) === path.resolve(snapshotPath)) throw new Error('Settings and snapshot paths must differ');
  if (command === 'set' && (typeof wrapperPath !== 'string' || !wrapperPath)) throw new Error('set requires a wrapper path');
  const current = readRegular(settingsPath, command !== 'restore');
  const text = current?.text ?? '{}\n';
  const parsed = parseSettings(text);
  if (command === 'inspect') return parsed.present ? JSON.stringify(parsed.value) : '__ABSENT__';
  const oldSnapshot = decodeSnapshot(snapshotPath);
  if (command === 'restore') {
    if (!oldSnapshot) throw new Error(`Settings snapshot does not exist: ${snapshotPath}`);
    if (!parsed.present || parsed.value !== oldSnapshot.installedValue) {
      throw new Error('Settings conflict: current CLI setting differs from installed value; snapshot retained');
    }
    atomicWrite(settingsPath, editSetting(text, oldSnapshot.originalPresent, oldSnapshot.originalValue), current.mode);
    fs.unlinkSync(snapshotPath);
    return undefined;
  }
  if (oldSnapshot && (!parsed.present || parsed.value !== oldSnapshot.installedValue)) {
    throw new Error('Settings conflict: current CLI setting differs from installed value; snapshot retained');
  }
  const snapshot = oldSnapshot ? { ...oldSnapshot, installedValue: wrapperPath } : {
    originalPresent: parsed.present,
    originalValue: parsed.present ? parsed.value : null,
    installedValue: wrapperPath,
  };
  const next = editSetting(text, true, wrapperPath);
  atomicWrite(snapshotPath, JSON.stringify(snapshot) + '\n', 0o600);
  try {
    atomicWrite(settingsPath, next, current?.mode ?? 0o600);
  } catch (error) {
    // No settings rename occurred: restore the previous ownership record as well.
    if (oldSnapshot) atomicWrite(snapshotPath, JSON.stringify(oldSnapshot) + '\n', 0o600);
    else fs.unlinkSync(snapshotPath);
    throw error;
  }
  return undefined;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const result = run(...process.argv.slice(2));
    if (result !== undefined) process.stdout.write(result + '\n');
  } catch (error) {
    console.error(`codex-background settings: ${error.message}`);
    process.exitCode = 1;
  }
}
