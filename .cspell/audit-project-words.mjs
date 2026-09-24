#!/usr/bin/env node

// Audit the project dictionary without relying on another repository.
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const dictionaryName = process.argv[2];
if (!dictionaryName) {
  console.error('usage: audit-project-words.mjs DICTIONARY_NAME');
  process.exit(2);
}

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = resolve(scriptDirectory, '..');
const dictionaryPath = resolve(scriptDirectory, 'project-words.txt');
const cspellEntryPoint = resolve(scriptDirectory, 'node_modules/cspell/bin.mjs');
const lines = readFileSync(dictionaryPath, 'utf8').split(/\r?\n/u);
const entries = lines
  .map((line) => line.trim())
  .filter((line) => line !== '' && !line.startsWith('#'));

const key = (word) => word.normalize('NFC').toLowerCase();
const compare = (left, right) => {
  const leftKey = key(left);
  const rightKey = key(right);
  return leftKey < rightKey ? -1 : leftKey > rightKey ? 1 : 0;
};

const errors = [];
if (entries.length === 0) {
  errors.push('dictionary has no entries');
}
const firstEntry = lines.findIndex(
  (line) => line.trim() !== '' && !line.trim().startsWith('#'),
);
if (
  lines.slice(firstEntry + 1).some((line) => line.trim().startsWith('#'))
) {
  errors.push('comments must precede all dictionary entries');
}

const seen = new Map();
for (const line of lines) {
  const entry = line.trim();
  if (entry === '' || entry.startsWith('#')) {
    continue;
  }
  if (entry !== line) {
    errors.push(`entry has leading or trailing whitespace: ${JSON.stringify(line)}`);
  }
  if (/\s/u.test(entry)) {
    errors.push(`entry contains whitespace: ${JSON.stringify(entry)}`);
  }
  const entryKey = key(entry);
  if (seen.has(entryKey)) {
    errors.push(
      `case-insensitive duplicate: ${JSON.stringify(seen.get(entryKey))} and ${JSON.stringify(entry)}`,
    );
  } else {
    seen.set(entryKey, entry);
  }
}

const sorted = [...entries].sort(compare);
for (let index = 0; index < entries.length; index += 1) {
  if (entries[index] !== sorted[index]) {
    errors.push(
      `dictionary is not case-insensitively sorted: expected ${JSON.stringify(sorted[index])} at entry ${index + 1}`,
    );
    break;
  }
}

const cspell = spawnSync(
  process.execPath,
  [
    cspellEntryPoint,
    'lint',
    '--dot',
    '--no-progress',
    '--no-summary',
    '--words-only',
    '--unique',
    '--disable-dictionary',
    dictionaryName,
    '.',
  ],
  {
    cwd: repositoryRoot,
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
  },
);

if (cspell.error || cspell.stderr.trim() !== '' || ![0, 1].includes(cspell.status)) {
  const detail = cspell.error?.message ?? cspell.stderr.trim();
  errors.push(`CSpell audit failed: ${detail || `exit status ${cspell.status}`}`);
} else {
  const missing = new Set();
  for (const word of cspell.stdout.split(/\r?\n/u).filter(Boolean)) {
    const wordKey = key(word);
    missing.add(wordKey);
    missing.add(wordKey.replace(/['’]s$/u, ''));
  }
  const unnecessary = entries.filter((entry) => !missing.has(key(entry)));
  if (unnecessary.length > 0) {
    errors.push(
      `entries unused or accepted by another rule:\n  ${unnecessary.join('\n  ')}`,
    );
  }
}

if (errors.length > 0) {
  console.error(`project-word audit failed:\n- ${errors.join('\n- ')}`);
  process.exit(1);
}

console.log(`Project-word audit passed: ${entries.length} entries.`);
