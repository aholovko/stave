'use strict';

// Run integration against source or --vsix in an isolated profile; retain logs and receipts for diagnosing failures.
// Without STAVE_VSCODE_EXECUTABLE, the runner downloads a stable editor into .vscode-test/.

const { spawn, spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const extensionRoot = path.resolve(__dirname, '..');
const repositoryRoot = path.resolve(extensionRoot, '..', '..');
const fixture = path.join(repositoryRoot, 'testdata', 'fixture-pkg');
const fixtureFiles = ['demo.go', 'demo_test.go', 'external_test.go', 'incomplete_test.go'];
const windows = process.platform === 'win32';
const linux = process.platform === 'linux';

// Prevent background updates and save-time edits from affecting the test.
const profileSettings = {
  'telemetry.telemetryLevel': 'off',
  'update.mode': 'none',
  'extensions.autoCheckUpdates': false,
  'extensions.autoUpdate': false,
  'git.enabled': false,
  'window.restoreWindows': 'none',
  'workbench.startupEditor': 'none',
  'files.autoSave': 'off',
  'files.hotExit': 'off',
  'files.trimTrailingWhitespace': false,
  'files.insertFinalNewline': false,
  'files.trimFinalNewlines': false,
  'editor.formatOnSave': false,
  'editor.codeActionsOnSave': {},
};

// Defaults match make editor-test. An empty STAVE_EDITOR_BUNDLE_B skips model switching.
function readSettings(env) {
  const build = (...parts) => path.join(repositoryRoot, 'build', ...parts);
  return {
    executable: env.STAVE_VSCODE_EXECUTABLE || '',
    server: path.resolve(env.STAVE_SERVER_PATH || build(windows ? 'stave.exe' : 'stave')),
    python: path.resolve(
      env.STAVE_RUNTIME_PYTHON ||
        path.join(repositoryRoot, 'infer', '.venv', ...(windows ? ['Scripts', 'python.exe'] : ['bin', 'python']))
    ),
    bundle: path.resolve(env.STAVE_EDITOR_BUNDLE || build('toy', 'toy-S')),
    bundleB: env.STAVE_EDITOR_BUNDLE_B === '' ? '' : path.resolve(env.STAVE_EDITOR_BUNDLE_B || build('toy', 'toy-B')),
    root: path.resolve(env.STAVE_EDITOR_TEST_ROOT || os.tmpdir()),
    timeoutMs: Number(env.STAVE_EDITOR_TIMEOUT_MS) || 300000,
  };
}

function readArguments(argv) {
  if (argv.length === 0) {
    return { vsix: '' };
  }
  if (argv.length === 2 && argv[0] === '--vsix') {
    return { vsix: path.resolve(argv[1]) };
  }
  throw new Error(`usage: node test/run.js [--vsix FILE], not ${argv.join(' ')}`);
}

function preflight(settings) {
  if (settings.vsix !== '') {
    requireExists(settings.vsix, 'the extension package', 'make it with make vsix, or name another with --vsix');
  }
  requireExecutable(settings.server, 'the stave server', 'build it, or set STAVE_SERVER_PATH');
  requireExecutable(settings.python, 'the runtime interpreter', 'run uv sync in infer/, or set STAVE_RUNTIME_PYTHON');
  requireExists(settings.bundle, 'the model bundle', 'make the toy bundles, or set STAVE_EDITOR_BUNDLE');
  if (settings.bundleB !== '') {
    requireExists(
      settings.bundleB,
      'the second model bundle',
      'make the toy bundles, set STAVE_EDITOR_BUNDLE_B, or set it empty to skip the model switch'
    );
  }
  if (settings.executable !== '') {
    requireExecutable(settings.executable, 'the VS Code executable', 'set STAVE_VSCODE_EXECUTABLE');
  }
  if (!fs.existsSync(path.join(extensionRoot, 'node_modules', 'vscode-languageclient', 'package.json'))) {
    throw new Error(`vscode-languageclient is not installed; run npm ci --ignore-scripts in ${extensionRoot}`);
  }
}

function requireExecutable(file, what, remedy) {
  try {
    if (!fs.statSync(file).isFile()) {
      throw new Error('it is not a regular file');
    }
    fs.accessSync(file, fs.constants.X_OK);
  } catch (error) {
    throw new Error(`${what} is not usable at ${file} (${error.message}); ${remedy}`);
  }
}

function requireExists(file, what, remedy) {
  if (!fs.existsSync(file)) {
    throw new Error(`${what} does not exist at ${file}; ${remedy}`);
  }
}

async function resolveEditor(settings) {
  if (settings.executable !== '') {
    return settings.executable;
  }
  const { downloadAndUnzipVSCode } = require('@vscode/test-electron');
  return downloadAndUnzipVSCode({ version: 'stable', cachePath: path.join(extensionRoot, '.vscode-test') });
}

function writeJson(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

// VS Code's profile socket must fit Unix path limits; Linux uses XDG_RUNTIME_DIR when set.
function requireSocketFits(root) {
  const limit = { darwin: 103, linux: 107 }[process.platform];
  if (!limit || (process.platform === 'linux' && process.env.XDG_RUNTIME_DIR)) {
    return;
  }
  const socket = path.join(root, 'stave-XXXXXX', 'ud', '1.00-main.sock');
  if (Buffer.byteLength(socket) > limit) {
    throw new Error(
      `${root} is too long: the editor's profile socket would need ${Buffer.byteLength(socket)} bytes and the ` +
        `platform allows ${limit}; choose a shorter STAVE_EDITOR_TEST_ROOT`
    );
  }
}

// Read the installed manifest so both zip and directory bundles use the same ID lookup.
function addModel(settings, run, bundle) {
  const modelsDir = path.join(run.home, 'models');
  const installed = () => (fs.existsSync(modelsDir) ? fs.readdirSync(modelsDir).filter((name) => !name.startsWith('.')) : []);
  const before = new Set(installed());
  const result = spawnSync(settings.server, ['model', 'add', bundle], {
    env: { ...process.env, STAVE_HOME: run.home, STAVE_RUNTIME_PYTHON: settings.python },
    encoding: 'utf8',
  });
  fs.appendFileSync(run.modelsLog, `$ stave model add ${bundle}\n${result.stdout ?? ''}${result.stderr ?? ''}`);
  if (result.error || result.status !== 0) {
    const reason = result.error ? result.error.message : `exit ${result.status}: ${(result.stderr ?? '').trim()}`;
    throw new Error(`stave model add ${bundle} failed (${reason})`);
  }
  const added = installed().filter((name) => !before.has(name));
  if (added.length !== 1) {
    throw new Error(`stave model add ${bundle} installed ${added.length} bundles under ${modelsDir}, want 1`);
  }
  return JSON.parse(fs.readFileSync(path.join(modelsDir, added[0], 'manifest.json'), 'utf8')).id;
}

// Use the editor's own installer to test the packaged extension as users install it.
function installPackage(executable, run, vsix) {
  const { resolveCliPathFromVSCodeExecutablePath } = require('@vscode/test-electron');
  const cli = resolveCliPathFromVSCodeExecutablePath(executable);
  const args = [`--extensions-dir=${run.extensions}`, `--user-data-dir=${run.userData}`, '--install-extension', vsix];
  // On Windows the CLI is a .cmd file, which Node runs only through the shell, and the shell needs the quotes.
  const result = windows
    ? spawnSync(`"${cli}"`, args.map((argument) => `"${argument}"`), { shell: true, encoding: 'utf8' })
    : spawnSync(cli, args, { encoding: 'utf8' });
  fs.writeFileSync(run.installLog, `$ ${[cli, ...args].join(' ')}\n${result.stdout ?? ''}${result.stderr ?? ''}`);
  if (result.error || result.status !== 0) {
    const reason = result.error ? result.error.message : `exit ${result.status}: ${(result.stderr ?? '').trim()}`;
    throw new Error(`${cli} --install-extension ${vsix} failed (${reason})`);
  }
}

function prepareRun(settings) {
  fs.mkdirSync(settings.root, { recursive: true });
  // Use one canonical path across the editor, server and assertions.
  const root = fs.realpathSync(settings.root);
  requireSocketFits(root);
  const dir = fs.mkdtempSync(path.join(root, 'stave-'));
  const workspace = path.join(dir, 'stave workspace');
  const run = {
    dir,
    workspace,
    package: path.join(workspace, 'fixture-pkg'),
    home: path.join(dir, 'home'),
    modelsLog: path.join(dir, 'models.log'),
    context: path.join(dir, 'context.json'),
    receipt: path.join(dir, 'receipt.json'),
    report: path.join(dir, 'runner.json'),
    log: path.join(dir, 'vscode.log'),
    userData: path.join(dir, 'ud'),
    extensions: path.join(dir, 'ext'),
    installLog: path.join(dir, 'install.log'),
    probe: path.join(dir, 'probe'),
  };
  console.log(`run directory: ${dir}`);

  fs.mkdirSync(run.package, { recursive: true });
  // Copy fixtures outside testdata so the TF path gate admits them.
  for (const name of fixtureFiles) {
    fs.copyFileSync(path.join(fixture, name), path.join(run.package, name));
  }
  // Leave serverArgs at its default to exercise normal installed-model resolution.
  writeJson(path.join(workspace, '.vscode', 'settings.json'), { 'stave.serverPath': settings.server });
  fs.writeFileSync(run.modelsLog, '');
  const backend = addModel(settings, run, settings.bundle);
  const switchTo = settings.bundleB === '' ? null : addModel(settings, run, settings.bundleB);
  writeJson(run.context, {
    files: fixtureFiles.map((name) => path.join(run.package, name)),
    receipt: run.receipt,
    cycles: 3,
    deadlineMs: settings.timeoutMs,
    server: settings.server,
    home: run.home,
    backend,
    switchTo,
    // Where the suite must find the extension it tests: null for the one under development.
    extensionsDir: settings.vsix === '' ? null : run.extensions,
  });
  writeJson(path.join(run.userData, 'User', 'settings.json'), profileSettings);
  fs.mkdirSync(run.extensions, { recursive: true });
  if (settings.vsix !== '') {
    writeJson(path.join(run.probe, 'package.json'), {
      name: 'stave-vsix-probe',
      publisher: 'stave',
      version: '0.0.0',
      engines: { vscode: '^1.82.0' },
    });
  }
  return run;
}

function launchArguments(settings, run) {
  const packaged = settings.vsix !== '';
  return [
    run.workspace,
    `--extensionDevelopmentPath=${packaged ? run.probe : extensionRoot}`,
    `--extensionTestsPath=${path.join(__dirname, 'suite.js')}`,
    // Isolate the test from the user's profile and extensions.
    `--user-data-dir=${run.userData}`,
    `--extensions-dir=${run.extensions}`,
    // The isolated extensions directory holds nothing but an installed package, which must stay enabled.
    ...(packaged ? [] : ['--disable-extensions']),
    '--disable-workspace-trust',
    '--skip-welcome',
    '--skip-release-notes',
    '--skip-add-to-recently-opened',
    '--sync=off',
    // A downloaded editor's sandbox helper is not setuid root, so Chromium aborts on Linux (microsoft/vscode#84238).
    ...(linux ? ['--no-sandbox', '--disable-gpu-sandbox'] : []),
  ];
}

function launch(settings, run, executable, args) {
  const env = { ...process.env, STAVE_HOME: run.home, STAVE_RUNTIME_PYTHON: settings.python };
  // Inherited Electron Node mode would prevent the GUI from starting.
  delete env.ELECTRON_RUN_AS_NODE;

  return new Promise((resolve) => {
    const log = fs.openSync(run.log, 'w');
    const child = spawn(executable, args, { env, stdio: ['ignore', log, log] });
    let timedOut = false;
    let kill;
    const deadline = setTimeout(() => {
      timedOut = true;
      child.kill('SIGTERM');
      kill = setTimeout(() => child.kill('SIGKILL'), 5000);
    }, settings.timeoutMs);
    const finish = (exit) => {
      clearTimeout(deadline);
      clearTimeout(kill);
      fs.closeSync(log);
      resolve({ timedOut, ...exit });
    };
    child.once('error', (error) => finish({ code: null, signal: null, error: error.message }));
    child.once('exit', (code, signal) => finish({ code, signal }));
  });
}

function readReceipt(run) {
  try {
    return JSON.parse(fs.readFileSync(run.receipt, 'utf8'));
  } catch {
    return undefined;
  }
}

// A success receipt is insufficient if the editor crashes afterward.
function judge(settings, run, exit) {
  const problems = [];
  const receipt = readReceipt(run);
  if (!receipt) {
    problems.push(`the suite never wrote a readable receipt to ${run.receipt}`);
  } else if (!receipt.ok) {
    problems.push(`the suite failed: ${receipt.error ? receipt.error.message : 'no error was recorded'}`);
  }
  if (exit.error) {
    problems.push(`the editor could not be started: ${exit.error}`);
  } else if (exit.timedOut) {
    problems.push(`the editor was stopped after ${settings.timeoutMs} ms`);
  } else if (exit.code !== 0) {
    problems.push(`the editor exited with code ${exit.code}${exit.signal ? ` on ${exit.signal}` : ''}`);
  }
  return { receipt, problems };
}

// CI keeps no run directory, so the editor's output is shown on failure.
function printEditorOutput(run) {
  const output = fs.readFileSync(run.log, 'utf8');
  console.error(output === '' ? `${run.log} is empty` : `${run.log}:\n${output.trimEnd()}`);
}

async function main() {
  const settings = { ...readSettings(process.env), ...readArguments(process.argv.slice(2)) };
  preflight(settings);
  const executable = await resolveEditor(settings);
  const run = prepareRun(settings);
  if (settings.vsix !== '') {
    installPackage(executable, run, settings.vsix);
  }

  const args = launchArguments(settings, run);
  const exit = await launch(settings, run, executable, args);
  const { receipt, problems } = judge(settings, run, exit);
  writeJson(run.report, { ok: problems.length === 0, executable, args, exit, problems });

  for (const step of receipt ? receipt.steps : []) {
    console.log(`  ${step.ok ? 'ok  ' : 'FAIL'}  ${step.name} (${step.ms} ms)`);
  }
  if (problems.length === 0) {
    console.log('stave editor test: PASS');
    return 0;
  }
  console.error(`stave editor test: FAIL, evidence kept in ${run.dir}`);
  for (const problem of problems) {
    console.error(`  - ${problem}`);
  }
  printEditorOutput(run);
  return 1;
}

main().then(
  (code) => {
    process.exitCode = code;
  },
  (error) => {
    console.error(`stave editor test: ${error.message}`);
    process.exitCode = 1;
  }
);
