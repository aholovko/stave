'use strict';

// Use stub executables and receipts to test the runner without opening an editor or loading a model.

const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');

const extensionRoot = path.resolve(__dirname, '..');
const runner = path.join(__dirname, 'run.js');
const fixture = path.resolve(extensionRoot, '..', '..', 'testdata', 'fixture-pkg');
const fixtureFiles = ['demo.go', 'demo_test.go', 'external_test.go', 'incomplete_test.go'];

// Windows cannot execute these shebang stubs; native editor behavior is covered by release verification.
const scripted = { skip: process.platform === 'win32' && 'the stub editor and server are #! scripts' };

const stubEditor = `#!${process.execPath}
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const argv = process.argv.slice(2);
fs.writeFileSync(
  process.env.STUB_EDITOR_MARKER,
  JSON.stringify({
    argv,
    electronRunAsNode: process.env.ELECTRON_RUN_AS_NODE ?? null,
    home: process.env.STAVE_HOME ?? null,
    python: process.env.STAVE_RUNTIME_PYTHON ?? null,
    installedFirst: process.env.STUB_CLI_LOG ? fs.existsSync(process.env.STUB_CLI_LOG) : null,
  })
);
const workspace = argv.find((argument) => !argument.startsWith('--'));
const context = JSON.parse(fs.readFileSync(path.join(workspace, '..', 'context.json'), 'utf8'));
const report = (receipt) => fs.writeFileSync(context.receipt, JSON.stringify(receipt));
const passed = { ok: true, steps: [{ name: 'open and save', ok: true, ms: 1 }] };
switch (process.env.STUB_EDITOR_MODE) {
  case 'pass':
    report(passed);
    break;
  case 'failed-step':
    report({
      ok: false,
      steps: [{ name: 'model switch', ok: false, ms: 1 }],
      error: { message: 'the next save did not name toy-B' },
    });
    process.exitCode = 1;
    break;
  case 'silent':
    break;
  case 'crash-after-pass':
    report(passed);
    process.exitCode = 3;
    break;
  case 'hang':
    setInterval(() => {}, 1000);
    break;
}
`;

const stubServer = `#!${process.execPath}
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const argv = process.argv.slice(2);
fs.appendFileSync(
  process.env.STUB_SERVER_LOG,
  JSON.stringify({ argv, home: process.env.STAVE_HOME ?? null, python: process.env.STAVE_RUNTIME_PYTHON ?? null }) + '\\n'
);
if (process.env.STUB_SERVER_FAIL) {
  console.error('bundle refused: stave_infer: saved seed 13 is not 12');
  process.exit(1);
}
if (argv[0] === 'model' && argv[1] === 'add') {
  const manifest = fs.readFileSync(path.join(argv[2], 'manifest.json'));
  const dir = path.join(process.env.STAVE_HOME, 'models', JSON.parse(manifest).id);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'manifest.json'), manifest);
  console.log('added ' + JSON.parse(manifest).id);
}
`;

const stubCli = `#!${process.execPath}
'use strict';
const fs = require('node:fs');
fs.writeFileSync(process.env.STUB_CLI_LOG, JSON.stringify({ argv: process.argv.slice(2) }));
if (process.env.STUB_CLI_FAIL) {
  console.error('Failed Installing Extensions: stave-vscode-0.1.0.vsix');
  process.exit(1);
}
`;

// Keep temporary paths short enough for the editor's Unix socket.
function sandbox(t) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'srt-')));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const executable = (name, body) => {
    const file = path.join(dir, name);
    fs.writeFileSync(file, body, { mode: 0o755 });
    return file;
  };
  const bundle = (id) => {
    const bundleDir = path.join(dir, id);
    fs.mkdirSync(bundleDir);
    fs.writeFileSync(path.join(bundleDir, 'manifest.json'), JSON.stringify({ format: 'stave-model-bundle-1', id }));
    return bundleDir;
  };
  return {
    dir,
    editor: executable('editor', stubEditor),
    server: executable('stave', stubServer),
    python: executable('python', ''),
    bundle: bundle('toy-S'),
    bundleB: bundle('toy-B'),
    root: path.join(dir, 'r'),
    marker: path.join(dir, 'marker.json'),
    calls: path.join(dir, 'calls.jsonl'),
  };
}

function runRunner(box, mode, overrides = {}, args = []) {
  const env = {
    ...process.env,
    STAVE_VSCODE_EXECUTABLE: box.editor,
    STAVE_SERVER_PATH: box.server,
    STAVE_RUNTIME_PYTHON: box.python,
    STAVE_EDITOR_BUNDLE: box.bundle,
    STAVE_EDITOR_BUNDLE_B: box.bundleB,
    STAVE_EDITOR_TEST_ROOT: box.root,
    STUB_EDITOR_MODE: mode,
    STUB_EDITOR_MARKER: box.marker,
    STUB_SERVER_LOG: box.calls,
    ...overrides,
  };
  for (const [name, value] of Object.entries(env)) {
    if (value === undefined) {
      delete env[name];
    }
  }
  delete env.NODE_TEST_CONTEXT;
  const result = spawnSync(process.execPath, [runner, ...args], { env, encoding: 'utf8', timeout: 60000 });
  const located = /^run directory: (.+)$/m.exec(result.stdout);
  return {
    status: result.status,
    stdout: result.stdout,
    stderr: result.stderr,
    runDir: located ? located[1] : undefined,
    marker: fs.existsSync(box.marker) ? JSON.parse(fs.readFileSync(box.marker, 'utf8')) : undefined,
    calls: fs.existsSync(box.calls)
      ? fs.readFileSync(box.calls, 'utf8').trim().split('\n').map((line) => JSON.parse(line))
      : [],
  };
}

function readContext(run) {
  return JSON.parse(fs.readFileSync(path.join(run.runDir, 'context.json'), 'utf8'));
}

function flagValue(argv, name) {
  const prefix = `--${name}=`;
  const values = argv.filter((argument) => argument.startsWith(prefix));
  assert.equal(values.length, 1, `${prefix} must be given exactly once: ${argv.join(' ')}`);
  return values[0].slice(prefix.length);
}

function isInside(child, parent) {
  const relative = path.relative(parent, child);
  return relative !== '' && !relative.startsWith('..') && !path.isAbsolute(relative);
}

test('passes when the editor exits 0 and the suite reports ok, keeping the run under the given root', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass');

  assert.equal(run.status, 0, run.stdout + run.stderr);
  assert.ok(isInside(run.runDir, box.root), `${run.runDir} is not under ${box.root}`);
  assert.equal(JSON.parse(fs.readFileSync(path.join(run.runDir, 'receipt.json'), 'utf8')).ok, true);
  assert.match(run.stdout, /open and save/);
});

test('copies the fixture package byte for byte into a workspace whose name has a space', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass');

  const workspace = run.marker.argv.find((argument) => !argument.startsWith('--'));
  assert.ok(isInside(workspace, run.runDir));
  assert.match(path.basename(workspace), / /);
  const context = readContext(run);
  assert.deepEqual(
    context.files,
    fixtureFiles.map((name) => path.join(workspace, 'fixture-pkg', name))
  );
  for (const name of fixtureFiles) {
    const copied = fs.readFileSync(path.join(workspace, 'fixture-pkg', name));
    assert.ok(copied.equals(fs.readFileSync(path.join(fixture, name))), `${name} is not copied byte for byte`);
  }
  assert.equal(context.cycles, 3);
});

test('configures the workspace with the server binary and leaves the default server arguments', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass');

  const workspace = run.marker.argv.find((argument) => !argument.startsWith('--'));
  const settings = JSON.parse(fs.readFileSync(path.join(workspace, '.vscode', 'settings.json'), 'utf8'));
  assert.deepEqual(settings, { 'stave.serverPath': box.server });
});

test('installs both bundles into a fresh data directory and names their ids from the manifests', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass');

  const home = path.join(run.runDir, 'home');
  assert.deepEqual(run.calls, [
    { argv: ['model', 'add', box.bundle], home, python: box.python },
    { argv: ['model', 'add', box.bundleB], home, python: box.python },
  ]);
  const context = readContext(run);
  assert.equal(context.server, box.server);
  assert.equal(context.home, home);
  assert.equal(context.backend, 'toy-S');
  assert.equal(context.switchTo, 'toy-B');
});

test('without a second bundle, installs one and plans no model switch', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass', { STAVE_EDITOR_BUNDLE_B: '' });

  assert.equal(run.status, 0, run.stdout + run.stderr);
  assert.deepEqual(
    run.calls.map((call) => call.argv),
    [['model', 'add', box.bundle]]
  );
  assert.equal(readContext(run).switchTo, null);
});

test('launches the editor with the data directory and the runtime interpreter in its environment', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass', { STAVE_HOME: path.join(box.dir, 'elsewhere') });

  assert.equal(run.marker.home, path.join(run.runDir, 'home'));
  assert.equal(run.marker.python, box.python);
});

test('stops before the editor when stave model add refuses a bundle, and says why', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass', { STUB_SERVER_FAIL: '1' });

  assert.equal(run.status, 1);
  assert.equal(run.marker, undefined, 'the editor was launched');
  assert.match(run.stderr, /model add/);
  assert.match(run.stderr, /saved seed 13 is not 12/);
});

for (const [variable, what] of [
  ['STAVE_EDITOR_BUNDLE', 'bundle'],
  ['STAVE_EDITOR_BUNDLE_B', 'bundle'],
  ['STAVE_RUNTIME_PYTHON', 'python'],
]) {
  test(`refuses to start when ${variable} names nothing`, (t) => {
    const box = sandbox(t);
    const missing = path.join(box.dir, 'absent', what);

    const run = runRunner(box, 'pass', { [variable]: missing });

    assert.equal(run.status, 1);
    assert.equal(run.marker, undefined, 'the editor was launched');
    assert.deepEqual(run.calls, [], 'stave was run');
    assert.ok(run.stderr.includes(missing), run.stderr);
    assert.ok(run.stderr.includes(variable), run.stderr);
  });
}

test('launches the editor on the workspace with an isolated profile and the required switches', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass', { ELECTRON_RUN_AS_NODE: '1' });

  const { argv } = run.marker;
  assert.equal(flagValue(argv, 'extensionDevelopmentPath'), extensionRoot);
  assert.equal(flagValue(argv, 'extensionTestsPath'), path.join(extensionRoot, 'test', 'suite.js'));
  const profile = [flagValue(argv, 'user-data-dir'), flagValue(argv, 'extensions-dir')];
  assert.notEqual(profile[0], profile[1]);
  for (const dir of profile) {
    assert.ok(isInside(dir, run.runDir), `${dir} is outside the fresh run directory`);
    assert.ok(fs.statSync(dir).isDirectory());
  }
  for (const flag of [
    '--disable-extensions',
    '--disable-workspace-trust',
    '--skip-welcome',
    '--skip-release-notes',
    '--skip-add-to-recently-opened',
    '--sync=off',
  ]) {
    assert.ok(argv.includes(flag), `${flag} is missing: ${argv.join(' ')}`);
  }
  assert.equal(argv.filter((argument) => !argument.startsWith('--')).length, 1);
  assert.equal(run.marker.electronRunAsNode, null, 'ELECTRON_RUN_AS_NODE reached the editor');
});

test('turns off the Chromium sandbox on Linux only, where a downloaded editor cannot use it', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass');

  const { argv } = run.marker;
  for (const flag of ['--no-sandbox', '--disable-gpu-sandbox']) {
    assert.equal(argv.includes(flag), process.platform === 'linux', `${flag}: ${argv.join(' ')}`);
  }
});

test('uses a fresh directory for every run', scripted, (t) => {
  const box = sandbox(t);

  const first = runRunner(box, 'pass');
  const second = runRunner(box, 'pass');

  assert.notEqual(first.runDir, second.runDir);
  assert.ok(fs.existsSync(path.join(first.runDir, 'receipt.json')), 'the earlier run was not retained');
});

test('falls back to the OS temporary directory without STAVE_EDITOR_TEST_ROOT', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass', { STAVE_EDITOR_TEST_ROOT: undefined });
  t.after(() => run.runDir && fs.rmSync(run.runDir, { recursive: true, force: true }));

  assert.equal(run.status, 0, run.stdout + run.stderr);
  assert.ok(isInside(run.runDir, fs.realpathSync(os.tmpdir())), `${run.runDir} is not under the OS temp directory`);
});

test('refuses to open the editor when the server binary does not exist', (t) => {
  const box = sandbox(t);
  const missing = path.join(box.dir, 'absent', 'stave');

  const run = runRunner(box, 'pass', { STAVE_SERVER_PATH: missing });

  assert.equal(run.status, 1);
  assert.equal(run.marker, undefined, 'the editor was launched');
  assert.ok(run.stderr.includes(missing), run.stderr);
  assert.match(run.stderr, /STAVE_SERVER_PATH/);
});

test('refuses to run when the editor executable does not exist', (t) => {
  const box = sandbox(t);
  const missing = path.join(box.dir, 'absent', 'Code');

  const run = runRunner(box, 'pass', { STAVE_VSCODE_EXECUTABLE: missing });

  assert.equal(run.status, 1);
  assert.deepEqual(run.calls, [], 'stave was run');
  assert.ok(run.stderr.includes(missing), run.stderr);
  assert.match(run.stderr, /STAVE_VSCODE_EXECUTABLE/);
});

test('fails and names the step when the suite reports a failure', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'failed-step');

  assert.equal(run.status, 1);
  assert.match(run.stdout + run.stderr, /model switch/);
  assert.match(run.stdout + run.stderr, /the next save did not name toy-B/);
});

test('fails when the editor exits 0 but the suite never reported', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'silent');

  assert.equal(run.status, 1);
  assert.match(run.stderr, /receipt/);
});

test('fails when the editor exits non-zero after an ok receipt', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'crash-after-pass');

  assert.equal(run.status, 1);
  assert.match(run.stderr, /exit(ed)? (with )?(code )?3/);
});

test('stops an editor that outlives the deadline and fails', scripted, (t) => {
  const box = sandbox(t);

  const started = Date.now();
  const run = runRunner(box, 'hang', { STAVE_EDITOR_TIMEOUT_MS: '400' });

  assert.equal(run.status, 1);
  assert.match(run.stderr, /400 ms/);
  assert.ok(Date.now() - started < 20000, 'the runner did not stop the editor promptly');
});

test('keeps the editor and model output of a run in its directory', scripted, (t) => {
  const box = sandbox(t);
  fs.appendFileSync(box.editor, "console.log('stub editor stdout'); console.error('stub editor stderr');\n");

  const run = runRunner(box, 'pass');

  const log = fs.readFileSync(path.join(run.runDir, 'vscode.log'), 'utf8');
  assert.match(log, /stub editor stdout/);
  assert.match(log, /stub editor stderr/);
  assert.match(fs.readFileSync(path.join(run.runDir, 'models.log'), 'utf8'), /added toy-S/);
});

test('shows the editor output when a run fails and leaves it out of a pass', scripted, (t) => {
  const box = sandbox(t);
  fs.appendFileSync(box.editor, "console.error('stub editor stderr');\n");

  const failed = runRunner(box, 'silent');
  const passed = runRunner(box, 'pass');

  assert.equal(failed.status, 1);
  assert.match(failed.stderr, /vscode\.log/);
  assert.match(failed.stderr, /stub editor stderr/);
  assert.equal(passed.status, 0, passed.stdout + passed.stderr);
  assert.doesNotMatch(passed.stdout + passed.stderr, /stub editor stderr/);
});

test('records how the editor was launched and how it ended', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'crash-after-pass');

  const report = JSON.parse(fs.readFileSync(path.join(run.runDir, 'runner.json'), 'utf8'));
  assert.equal(report.ok, false);
  assert.equal(report.executable, box.editor);
  assert.deepEqual(report.args, run.marker.argv);
  assert.deepEqual(report.exit, { timedOut: false, code: 3, signal: null });
  assert.equal(report.problems.length, 1);
});

// Mirror the editor layout so the CLI resolver stays inside the test sandbox.
function packaged(box) {
  const { resolveCliPathFromVSCodeExecutablePath } = require('@vscode/test-electron');
  const editor = path.join(box.dir, 'Code.app', 'Contents', 'MacOS', 'editor');
  fs.mkdirSync(path.dirname(editor), { recursive: true });
  fs.writeFileSync(editor, stubEditor, { mode: 0o755 });
  const cli = resolveCliPathFromVSCodeExecutablePath(editor);
  assert.ok(isInside(cli, box.dir), `${cli} is outside the sandbox`);
  fs.mkdirSync(path.dirname(cli), { recursive: true });
  fs.writeFileSync(cli, stubCli, { mode: 0o755 });
  const vsix = path.join(box.dir, 'stave-vscode-0.1.0.vsix');
  fs.writeFileSync(vsix, 'PK');
  const log = path.join(box.dir, 'cli.json');
  return { editor, vsix, log, env: { STAVE_VSCODE_EXECUTABLE: editor, STUB_CLI_LOG: log } };
}

test('with --vsix, installs the package with the editor CLI into the isolated profile first', scripted, (t) => {
  const box = sandbox(t);
  const pkg = packaged(box);

  const run = runRunner(box, 'pass', pkg.env, ['--vsix', pkg.vsix]);

  assert.equal(run.status, 0, run.stdout + run.stderr);
  const { argv } = JSON.parse(fs.readFileSync(pkg.log, 'utf8'));
  const editorArgv = run.marker.argv;
  assert.deepEqual(argv, [
    `--extensions-dir=${flagValue(editorArgv, 'extensions-dir')}`,
    `--user-data-dir=${flagValue(editorArgv, 'user-data-dir')}`,
    '--install-extension',
    pkg.vsix,
  ]);
  assert.equal(run.marker.installedFirst, true, 'the editor started before the package was installed');
});

test('with --vsix, runs the suite against the installed package, not the source tree', scripted, (t) => {
  const box = sandbox(t);
  const pkg = packaged(box);

  const run = runRunner(box, 'pass', pkg.env, ['--vsix', pkg.vsix]);

  assert.equal(run.status, 0, run.stdout + run.stderr);
  const { argv } = run.marker;
  const development = flagValue(argv, 'extensionDevelopmentPath');
  assert.notEqual(development, extensionRoot);
  assert.ok(isInside(development, run.runDir), `${development} is outside the fresh run directory`);
  assert.equal(flagValue(argv, 'extensionTestsPath'), path.join(extensionRoot, 'test', 'suite.js'));
  assert.ok(!argv.includes('--disable-extensions'), 'the installed package would be disabled');
  assert.equal(readContext(run).extensionsDir, flagValue(argv, 'extensions-dir'));
});

test('without --vsix, the suite expects the extension under development', scripted, (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass');

  assert.equal(readContext(run).extensionsDir, null);
});

test('stops before the editor when the editor CLI cannot install the package, and says why', scripted, (t) => {
  const box = sandbox(t);
  const pkg = packaged(box);

  const run = runRunner(box, 'pass', { ...pkg.env, STUB_CLI_FAIL: '1' }, ['--vsix', pkg.vsix]);

  assert.equal(run.status, 1);
  assert.equal(run.marker, undefined, 'the editor was launched');
  assert.match(run.stderr, /--install-extension/);
  assert.match(run.stderr, /Failed Installing Extensions/);
});

test('refuses to start when --vsix names nothing', (t) => {
  const box = sandbox(t);
  const missing = path.join(box.dir, 'absent', 'stave-vscode-0.1.0.vsix');

  const run = runRunner(box, 'pass', {}, ['--vsix', missing]);

  assert.equal(run.status, 1);
  assert.equal(run.marker, undefined, 'the editor was launched');
  assert.deepEqual(run.calls, [], 'stave was run');
  assert.ok(run.stderr.includes(missing), run.stderr);
});

test('refuses an argument it does not know', (t) => {
  const box = sandbox(t);

  const run = runRunner(box, 'pass', {}, ['--package']);

  assert.equal(run.status, 1);
  assert.equal(run.marker, undefined, 'the editor was launched');
  assert.match(run.stderr, /usage: node test\/run\.js \[--vsix FILE\]/);
});

test(
  'refuses a root that would push the profile socket past the platform path limit',
  { skip: process.platform === 'win32' || Boolean(process.env.XDG_RUNTIME_DIR) },
  (t) => {
    const box = sandbox(t);
    const deep = path.join(box.root, 'd'.repeat(60), 'e'.repeat(60));

    const run = runRunner(box, 'pass', { STAVE_EDITOR_TEST_ROOT: deep });

    assert.equal(run.status, 1);
    assert.equal(run.marker, undefined, 'the editor was launched');
    assert.match(run.stderr, /socket/);
    assert.match(run.stderr, /STAVE_EDITOR_TEST_ROOT/);
  }
);
