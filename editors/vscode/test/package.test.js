'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { after, before, test } = require('node:test');
const { bundle } = require('../bundle');

const extensionRoot = path.resolve(__dirname, '..');
const manifest = require('../package.json');
let outdir;
let build;

before(async () => {
  outdir = fs.mkdtempSync(path.join(os.tmpdir(), 'stave-bundle-'));
  build = await bundle(outdir);
});

after(() => fs.rmSync(outdir, { recursive: true, force: true }));

test('the manifest names the release, its licence and its repository', () => {
  assert.equal(manifest.name, 'stave-vscode');
  assert.equal(manifest.publisher, 'stave');
  assert.match(manifest.version, /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
  assert.equal(manifest.license, 'Apache-2.0');
  assert.equal(manifest.repository.url, 'https://github.com/aholovko/stave');
  assert.deepEqual(Object.keys(manifest.dependencies), ['vscode-languageclient']);
  assert.deepEqual(Object.keys(manifest.contributes.configuration.properties), ['stave.serverPath', 'stave.serverArgs']);
  assert.equal(manifest.contributes.commands, undefined);
});

// Windows searches the workspace before PATH for a bare executable, so activation requires workspace trust.
test('the extension waits for workspace trust', () => {
  assert.equal(manifest.capabilities.untrustedWorkspaces.supported, false);
  assert.equal(manifest.capabilities.untrustedWorkspaces.restrictedConfigurations, undefined);
});

test('the package ships the bundle in place of the source and node_modules', () => {
  assert.equal(manifest.main, './out/extension.js');
  assert.equal(manifest.scripts['vscode:prepublish'], 'npm run bundle');
  assert.equal(manifest.scripts.pretest, 'npm run bundle');
  const ignored = fs.readFileSync(path.join(extensionRoot, '.vscodeignore'), 'utf8').split(/\r?\n/);
  for (const pattern of ['node_modules/**', 'extension.js', 'bundle.js']) {
    assert.ok(ignored.includes(pattern), `${pattern} is not in .vscodeignore`);
  }
});

test('the extension carries the repository licence unchanged', () => {
  const licence = fs.readFileSync(path.join(extensionRoot, 'LICENSE'));
  assert.ok(licence.equals(fs.readFileSync(path.join(extensionRoot, '..', '..', 'LICENSE'))));
});

test('the bundle carries the upstream Unix termination helper with executable permissions', () => {
  assert.ok(fs.statSync(path.join(outdir, 'extension.js')).size > 0);
  const helper = path.join(outdir, 'terminateProcess.sh');
  const upstream = require.resolve('vscode-languageclient/lib/node/terminateProcess.sh');
  assert.ok(fs.readFileSync(helper).equals(fs.readFileSync(upstream)));
  if (process.platform !== 'win32') {
    assert.equal(fs.statSync(helper).mode & 0o777, 0o755);
  }
});

test('third-party notices cover the actual bundle and retain the installed dependencies’ full licences', () => {
  const licences = {
    'balanced-match': 'LICENSE.md',
    'brace-expansion': 'LICENSE',
    minimatch: 'LICENSE',
    semver: 'LICENSE',
    'vscode-jsonrpc': 'License.txt',
    'vscode-languageclient': 'License.txt',
    'vscode-languageserver-protocol': 'License.txt',
    'vscode-languageserver-types': 'License.txt',
  };
  const bundled = new Set(
    Object.keys(build.metafile.inputs)
      .filter((file) => file.startsWith('node_modules/'))
      .map((file) => file.match(/^node_modules\/((?:@[^/]+\/)?[^/]+)/)[1])
  );
  assert.deepEqual([...bundled].sort(), Object.keys(licences));
  const normalize = (text) => text.replace(/\s+/g, ' ').trim();
  const sections = Object.entries(licences).map(([name, file]) => {
    const root = path.join(extensionRoot, 'node_modules', name);
    const dependency = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
    const licence = normalize(fs.readFileSync(path.join(root, file), 'utf8'));
    return `${name} ${dependency.version} (${dependency.license})\n\n${licence}`;
  });
  const expected = 'Third-party notices for the STAVE VS Code extension\n\n' + sections.join('\n\n---\n\n');
  const notices = fs.readFileSync(path.join(extensionRoot, 'THIRD_PARTY_NOTICES.txt'), 'utf8');
  assert.equal(normalize(notices), normalize(expected));
});
