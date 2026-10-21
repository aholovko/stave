'use strict';

// Test lifecycle and presentation without an extension host. Doubles model the pinned language client's behavior.

const assert = require('node:assert/strict');
const Module = require('node:module');
const path = require('node:path');
const { test } = require('node:test');

const manifest = require('../package.json');

const extensionPath = path.resolve(__dirname, '..', 'extension.js');

// Keep doubled lifecycle operations asynchronous so tests can exercise races.
function turn() {
  return new Promise((resolve) => setImmediate(resolve));
}

// Use manifest defaults for unset settings; freeze array copies to catch accidental mutation in tests.
function manifestDefault(key) {
  const properties = manifest.contributes?.configuration?.properties ?? {};
  const value = properties[key]?.default;
  return Array.isArray(value) ? Object.freeze([...value]) : value;
}

function createVscodeDouble(log, settings) {
  class EventEmitter {
    constructor() {
      this.listeners = new Set();
      this.event = (listener) => {
        this.listeners.add(listener);
        return { dispose: () => this.listeners.delete(listener) };
      };
    }

    fire(value) {
      for (const listener of [...this.listeners]) {
        listener(value);
      }
    }

    dispose() {
      this.listeners.clear();
    }
  }

  const subscribe = (listeners) => (listener) => {
    listeners.add(listener);
    return { dispose: () => listeners.delete(listener) };
  };

  return {
    StatusBarAlignment: { Left: 1, Right: 2 },
    EventEmitter,
    workspace: {
      getConfiguration(section) {
        return {
          get(key) {
            const full = `${section}.${key}`;
            return Object.hasOwn(settings, full) ? settings[full] : manifestDefault(full);
          },
        };
      },
      createFileSystemWatcher(glob) {
        const index = log.watchers.length;
        const watcher = {
          glob,
          disposed: false,
          dispose() {
            this.disposed = true;
            log.events.push(`dispose:watcher:${index}`);
          },
        };
        log.watchers.push(watcher);
        return watcher;
      },
      onDidChangeConfiguration(listener) {
        log.configurationListeners.add(listener);
        return { dispose: () => log.configurationListeners.delete(listener) };
      },
      onDidChangeTextDocument: subscribe(log.documentChangeListeners),
      onDidCloseTextDocument: subscribe(log.documentCloseListeners),
      // VS Code removes a closed document before firing its close event.
      get textDocuments() {
        return [...log.openDocuments];
      },
    },
    commands: {
      registerCommand(id, handler) {
        assert.equal(log.commands.has(id), false, `command ${id} registered twice`);
        log.commands.set(id, handler);
        return { dispose: () => log.commands.delete(id) };
      },
      async executeCommand(id, ...args) {
        const handler = log.commands.get(id);
        assert.ok(handler, `command '${id}' not found`);
        return handler(...args);
      },
    },
    window: {
      activeTextEditor: undefined,
      onDidChangeActiveTextEditor: subscribe(log.activeEditorListeners),
      createStatusBarItem(id, alignment, priority) {
        const item = {
          id,
          alignment,
          priority,
          name: '',
          text: '',
          tooltip: '',
          command: undefined,
          visible: false,
          disposed: false,
          show() {
            this.visible = true;
          },
          hide() {
            this.visible = false;
          },
          dispose() {
            this.disposed = true;
            this.visible = false;
          },
        };
        log.statusItems.push(item);
        return item;
      },
      createOutputChannel(name) {
        const channel = {
          name,
          lines: [],
          disposed: false,
          appendLine(line) {
            assert.equal(this.disposed, false, `wrote to the closed ${name} channel: ${line}`);
            this.lines.push(line);
          },
          show() {
            log.events.push(`show:channel:${name}`);
          },
          dispose() {
            this.disposed = true;
            log.events.push(`dispose:channel:${name}`);
          },
        };
        log.channels.push(channel);
        return channel;
      },
    },
  };
}

function createLanguageClientDouble(log) {
  const TransportKind = { stdio: 0, ipc: 1, pipe: 2, socket: 3 };
  // Both failed starts and completed stops report Stopped.
  const State = { Stopped: 1, Running: 2, Starting: 3 };

  class LanguageClient {
    constructor(id, name, serverOptions, clientOptions) {
      Object.assign(this, { id, name, serverOptions, clientOptions });
      this.index = log.clients.length;
      this.phase = 'initial';
      this.notificationHandlers = new Map();
      this.stateListeners = new Set();
      log.clients.push(this);
    }

    get state() {
      return this.phase === 'starting' ? State.Starting : this.phase === 'running' ? State.Running : State.Stopped;
    }

    // The real client retains handlers registered before start.
    onNotification(method, handler) {
      this.notificationHandlers.set(method, handler);
      return { dispose: () => this.notificationHandlers.delete(method) };
    }

    onDidChangeState(listener) {
      this.stateListeners.add(listener);
      return { dispose: () => this.stateListeners.delete(listener) };
    }

    changeState(oldState, newState) {
      for (const listener of [...this.stateListeners]) {
        listener({ oldState, newState });
      }
    }

    // Match the client's argument handling, including the extra --stdio from an explicit transport.
    get spawned() {
      const { command, args = [], transport } = this.serverOptions;
      const argv = [...args];
      if (transport === TransportKind.stdio) {
        argv.push('--stdio');
      } else if (transport !== undefined) {
        throw new Error('the double models only the stdio transports');
      }
      return { command, argv };
    }

    // Repeated starts await the same initialization, as the pinned client does.
    start() {
      this.startPromise ??= this.initialize();
      return this.startPromise;
    }

    async initialize() {
      this.phase = 'starting';
      log.live += 1;
      log.maxLive = Math.max(log.maxLive, log.live);
      log.events.push(`start:${this.index}`);
      this.changeState(State.Stopped, State.Starting);
      await turn();
      if (log.failStart.has(this.index)) {
        this.phase = 'startFailed';
        log.live -= 1;
        this.changeState(State.Starting, State.Stopped);
        // handleChildProcessStartError in lib/node/main.js rejects with a string, not an Error.
        const { command } = this.serverOptions;
        throw `Launching server using command ${command} failed. Error: spawn ${command} ENOENT`;
      }
      this.phase = 'running';
      log.events.push(`started:${this.index}`);
      this.changeState(State.Starting, State.Running);
    }

    // The pinned client restarts itself after a crash, outside the extension's lifecycle queue.
    crash() {
      assert.equal(this.phase, 'running');
      this.phase = 'initial';
      log.live -= 1;
      this.changeState(State.Running, State.Stopped);
      this.startPromise = undefined;
      return this.start();
    }

    needsStop() {
      return this.phase === 'starting' || this.phase === 'running';
    }

    // Match the client's refusal to stop while starting.
    async stop() {
      if (this.phase === 'initial' || this.phase === 'stopped') {
        return;
      }
      if (this.phase !== 'running') {
        throw new Error(`Client is not running and can't be stopped. It's current state is: ${this.phase}`);
      }
      this.phase = 'stopping';
      log.events.push(`stop:${this.index}`);
      this.changeState(State.Running, State.Stopped);
      await turn();
      this.phase = 'stopped';
      this.startPromise = undefined;
      log.live -= 1;
      if (log.failStop.has(this.index)) {
        // A shutdown timeout still leaves the client in Stopped.
        throw new Error('Stopping the server timed out');
      }
      log.events.push(`stopped:${this.index}`);
    }
  }

  return { LanguageClient, TransportKind, State };
}

// Patch module resolution only while loading the extension to keep doubles local to the test.
function loadExtension(doubles) {
  const load = Module._load;
  Module._load = function (request, ...rest) {
    if (request === 'vscode') {
      return doubles.vscode;
    }
    if (request === 'vscode-languageclient/node') {
      return doubles.languageClient;
    }
    return load.call(this, request, ...rest);
  };
  try {
    delete require.cache[extensionPath];
    return require(extensionPath);
  } finally {
    Module._load = load;
  }
}

// failStart lists the servers, in creation order, whose executable cannot be spawned. failStop lists the servers that
// do not answer the shutdown request in time.
function createHarness(settings = {}, { failStart = [], failStop = [] } = {}) {
  const log = {
    events: [],
    clients: [],
    watchers: [],
    channels: [],
    statusItems: [],
    commands: new Map(),
    configurationListeners: new Set(),
    activeEditorListeners: new Set(),
    documentChangeListeners: new Set(),
    documentCloseListeners: new Set(),
    openDocuments: [],
    failStart: new Set(failStart),
    failStop: new Set(failStop),
    live: 0,
    maxLive: 0,
  };
  const vscode = createVscodeDouble(log, settings);
  const languageClient = createLanguageClientDouble(log);
  const extension = loadExtension({ vscode, languageClient });

  // Configuration events affect parent and child sections as well as the changed key.
  function fireConfigurationChange(keys) {
    const event = {
      affectsConfiguration(section) {
        return keys.some(
          (key) => key === section || key.startsWith(`${section}.`) || section.startsWith(`${key}.`)
        );
      },
    };
    for (const listener of [...log.configurationListeners]) {
      listener(event);
    }
  }

  function show(document) {
    if (document !== undefined && !document.isClosed && !log.openDocuments.includes(document)) {
      log.openDocuments.push(document);
    }
    vscode.window.activeTextEditor = document === undefined ? undefined : { document };
    for (const listener of [...log.activeEditorListeners]) {
      listener(vscode.window.activeTextEditor);
    }
  }
  function serverStatus(payload, client = log.clients.at(-1)) {
    const handler = client.notificationHandlers.get('stave/status');
    assert.ok(handler, `server ${client.index} registered no stave/status handler`);
    handler(payload);
  }
  function changed(document) {
    document.version += 1;
    for (const listener of [...log.documentChangeListeners]) {
      listener({ document, contentChanges: [] });
    }
  }
  function closed(document) {
    document.isClosed = true;
    log.openDocuments = log.openDocuments.filter((open) => open !== document);
    for (const listener of [...log.documentCloseListeners]) {
      listener(document);
    }
  }
  const item = () => log.statusItems[0];

  return {
    extension,
    vscode,
    log,
    settings,
    State: languageClient.State,
    fireConfigurationChange,
    show,
    serverStatus,
    changed,
    closed,
    item,
    context: { subscriptions: [] },
  };
}

// Restarts run in the background, so tests wait for an observable outcome, bounded in event-loop turns.
async function waitFor(predicate, description) {
  for (let turns = 0; turns < 1000; turns += 1) {
    if (predicate()) {
      return;
    }
    await turn();
  }
  assert.fail(`gave up waiting for ${description}`);
}

// Doubled operations take one turn; twenty quiet turns expose pending lifecycle work without a fixed sleep.
async function settled(log) {
  let quiet = 0;
  let seen = log.events.length;
  await waitFor(() => {
    const busy = log.clients.some((client) => client.phase === 'starting' || client.phase === 'stopping');
    quiet = busy || log.events.length !== seen ? 0 : quiet + 1;
    seen = log.events.length;
    return quiet === 20;
  }, 'the extension to become idle');
}

test('starts the default command line, stave lsp --stdio, exactly once', async () => {
  const h = createHarness();

  await h.extension.activate(h.context);

  assert.equal(h.log.clients.length, 1);
  assert.deepEqual(h.log.clients[0].spawned, { command: 'stave', argv: ['lsp', '--stdio'] });
  assert.deepEqual(h.log.events, ['start:0', 'started:0']);
});

test('passes stave.serverPath and stave.serverArgs to the server verbatim, advisory flags included', async () => {
  const h = createHarness({
    'stave.serverPath': '/opt/stave/build/stave',
    'stave.serverArgs': ['lsp', '-advisory', 'recorded', '-advisory-recorded', '/tmp/a5 recorded.json', '--stdio'],
  });

  await h.extension.activate(h.context);

  assert.deepEqual(h.log.clients[0].spawned, {
    command: '/opt/stave/build/stave',
    argv: ['lsp', '-advisory', 'recorded', '-advisory-recorded', '/tmp/a5 recorded.json', '--stdio'],
  });
});

test('synchronizes Go files on disk and forwards **/*.go file events from its own watcher', async () => {
  const h = createHarness();

  await h.extension.activate(h.context);

  const { clientOptions } = h.log.clients[0];
  assert.deepEqual(clientOptions.documentSelector, [{ scheme: 'file', language: 'go' }]);
  assert.deepEqual(
    h.log.watchers.map((watcher) => watcher.glob),
    ['**/*.go']
  );
  assert.equal(clientOptions.synchronize.fileEvents, h.log.watchers[0]);
  assert.equal(h.log.watchers[0].disposed, false);
});

test('deactivate resolves after the server has shut down and the watcher is disposed', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);

  await h.extension.deactivate();

  assert.equal(h.log.clients[0].phase, 'stopped');
  assert.equal(h.log.watchers[0].disposed, true);
});

const restartingChanges = [
  {
    key: 'stave.serverPath',
    value: '/new/build/stave',
    want: { command: '/new/build/stave', argv: ['lsp', '--stdio'] },
  },
  {
    key: 'stave.serverArgs',
    value: ['lsp', '-advisory', 'disabled', '--stdio'],
    want: { command: 'stave', argv: ['lsp', '-advisory', 'disabled', '--stdio'] },
  },
];

for (const { key, value, want } of restartingChanges) {
  test(`a change of ${key} replaces the server with one started from the new settings`, async () => {
    const h = createHarness();
    await h.extension.activate(h.context);

    h.settings[key] = value;
    h.fireConfigurationChange([key]);
    await waitFor(() => h.log.events.includes('started:1'), 'the replacement server to start');

    assert.deepEqual(h.log.clients[1].spawned, want);
    assert.equal(h.log.clients[0].phase, 'stopped');
    assert.deepEqual(
      h.log.watchers.map((watcher) => watcher.disposed),
      [true, false]
    );
    assert.equal(h.log.clients[1].clientOptions.synchronize.fileEvents, h.log.watchers[1]);
    assert.ok(
      h.log.events.indexOf('stopped:0') < h.log.events.indexOf('start:1'),
      `the old server must be gone before the new one is spawned: ${h.log.events.join(' ')}`
    );
  });
}

test('leaves the server alone when an unrelated setting changes', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);

  h.fireConfigurationChange(['editor.fontSize', 'staveother.serverPath']);
  await settled(h.log);

  assert.equal(h.log.clients.length, 1);
  assert.equal(h.log.clients[0].phase, 'running');
  assert.equal(h.log.watchers[0].disposed, false);
});

test('every server logs to one STAVE channel, which closes only after the last shutdown', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  h.fireConfigurationChange(['stave.serverArgs']);
  await waitFor(() => h.log.events.includes('started:1'), 'the replacement server to start');

  assert.deepEqual(
    h.log.channels.map((channel) => [channel.name, channel.disposed]),
    [['STAVE', false]]
  );
  for (const client of h.log.clients) {
    assert.equal(client.clientOptions.outputChannel, h.log.channels[0]);
  }

  await h.extension.deactivate();

  assert.equal(h.log.channels[0].disposed, true);
  assert.ok(
    h.log.events.indexOf('stopped:1') < h.log.events.indexOf('dispose:channel:STAVE'),
    `the channel must outlive the shutdown that may still log to it: ${h.log.events.join(' ')}`
  );
});

test('a server that cannot be spawned is reported and replaced once the settings are corrected', async () => {
  const h = createHarness({ 'stave.serverPath': '/missing/stave' }, { failStart: [0] });

  await h.extension.activate(h.context);
  h.settings['stave.serverPath'] = '/fixed/stave';
  h.fireConfigurationChange(['stave.serverPath']);
  await settled(h.log);

  assert.match(h.log.channels[0].lines.join('\n'), /spawn \/missing\/stave ENOENT/);
  assert.deepEqual(
    h.log.clients.map((client) => client.phase),
    ['startFailed', 'running']
  );
  assert.deepEqual(h.log.clients[1].spawned, { command: '/fixed/stave', argv: ['lsp', '--stdio'] });
  assert.deepEqual(
    h.log.watchers.map((watcher) => watcher.disposed),
    [true, false]
  );
});

test('deactivate after a failed start releases the watcher and the channel', async () => {
  const h = createHarness({}, { failStart: [0] });
  await h.extension.activate(h.context);

  await h.extension.deactivate();

  assert.equal(h.log.clients[0].phase, 'startFailed');
  assert.equal(h.log.watchers[0].disposed, true);
  assert.equal(h.log.channels[0].disposed, true);
});

test('a shutdown that times out is reported, releases the watcher and lets the replacement start', async () => {
  const h = createHarness({}, { failStop: [0] });
  await h.extension.activate(h.context);

  h.fireConfigurationChange(['stave.serverArgs']);
  await settled(h.log);

  assert.match(h.log.channels[0].lines.join('\n'), /Stopping the server timed out/);
  assert.deepEqual(
    h.log.clients.map((client) => client.phase),
    ['stopped', 'running']
  );
  assert.deepEqual(
    h.log.watchers.map((watcher) => watcher.disposed),
    [true, false]
  );
});

test('stops reacting to configuration changes once VS Code disposes the extension context', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);

  // VS Code disposes the subscriptions right after calling deactivate, without awaiting its promise.
  const deactivation = h.extension.deactivate();
  for (const subscription of h.context.subscriptions) {
    subscription.dispose();
  }
  await deactivation;
  h.fireConfigurationChange(['stave.serverArgs']);
  await settled(h.log);

  assert.deepEqual(
    h.log.clients.map((client) => client.phase),
    ['stopped']
  );
});

test('a change during startup waits for that start before it replaces the server', async () => {
  const h = createHarness();
  const activation = h.extension.activate(h.context);

  h.settings['stave.serverArgs'] = ['lsp', '-tags', 'late', '--stdio'];
  h.fireConfigurationChange(['stave.serverArgs']);
  await activation;
  await settled(h.log);

  assert.equal(h.log.maxLive, 1);
  assert.deepEqual(
    h.log.clients.map((client) => client.phase),
    ['stopped', 'running']
  );
  assert.deepEqual(h.log.clients[1].spawned.argv, ['lsp', '-tags', 'late', '--stdio']);
  assert.deepEqual(
    h.log.watchers.map((watcher) => watcher.disposed),
    [true, false]
  );
});

for (const fails of [false, true]) {
  const outcome = fails ? 'fails' : 'succeeds';

  test(`a settings change waits for an automatic restart that ${outcome}`, async () => {
    const h = createHarness();
    await h.extension.activate(h.context);
    const old = h.log.clients[0];
    if (fails) {
      h.log.failStart.add(0);
    }

    const restarted = old.crash().catch(() => {});
    assert.equal(old.state, h.State.Starting);
    h.settings['stave.serverArgs'] = ['lsp', '-advisory', 'disabled', '--stdio'];
    h.fireConfigurationChange(['stave.serverArgs']);
    await restarted;
    await settled(h.log);

    assert.deepEqual(h.log.clients.map((client) => client.phase), [fails ? 'startFailed' : 'stopped', 'running']);
    assert.equal(h.log.live, 1);
    assert.equal(h.log.maxLive, 1);
    assert.deepEqual(h.log.clients[1].spawned.argv, h.settings['stave.serverArgs']);
    assert.deepEqual(h.log.watchers.map((watcher) => watcher.disposed), [true, false]);
    assert.doesNotMatch(h.log.channels[0].lines.join('\n'), /can't be stopped/);
    if (fails) {
      assert.match(h.log.channels[0].lines.join('\n'), /ENOENT/);
    }
  });

  test(`deactivation waits for an automatic restart that ${outcome}`, async () => {
    const h = createHarness();
    await h.extension.activate(h.context);
    const old = h.log.clients[0];
    if (fails) {
      h.log.failStart.add(0);
    }

    const restarted = old.crash().catch(() => {});
    assert.equal(old.state, h.State.Starting);
    await h.extension.deactivate();
    await restarted;

    assert.equal(old.phase, fails ? 'startFailed' : 'stopped');
    assert.equal(h.log.live, 0);
    assert.equal(h.log.maxLive, 1);
    assert.equal(h.log.watchers[0].disposed, true);
    assert.equal(h.log.channels[0].disposed, true);
    assert.doesNotMatch(h.log.channels[0].lines.join('\n'), /can't be stopped/);
    if (fails) {
      assert.match(h.log.channels[0].lines.join('\n'), /ENOENT/);
    }
  });
}

test('rapid successive changes never run two servers at once and end on the latest settings', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);

  for (const tag of ['first', 'second', 'third']) {
    h.settings['stave.serverArgs'] = ['lsp', '-tags', tag, '--stdio'];
    h.fireConfigurationChange(['stave.serverArgs']);
  }
  await settled(h.log);

  assert.equal(h.log.maxLive, 1);
  const last = h.log.clients.at(-1);
  assert.equal(last.phase, 'running');
  assert.deepEqual(last.spawned.argv, ['lsp', '-tags', 'third', '--stdio']);
  for (const client of h.log.clients.slice(0, -1)) {
    assert.equal(client.phase, 'stopped', `server ${client.index} was left behind`);
  }
  assert.deepEqual(
    h.log.watchers.map((watcher) => watcher.disposed),
    [...h.log.watchers.slice(0, -1).map(() => true), false]
  );
});

test('registers no code action middleware and no command other than showing the output', async () => {
  const h = createHarness();

  await h.extension.activate(h.context);

  assert.equal(h.log.clients[0].clientOptions.middleware, undefined);
  assert.deepEqual([...h.log.commands.keys()], ['stave.showOutput']);
});

// Reopening creates a new document object whose version may repeat.
function openDocument(uri = 'file:///pkg/demo.go', version = 1) {
  return { uri: { toString: () => uri, scheme: 'file' }, languageId: 'go', version, isClosed: false };
}

// The server's status for a test file without test functions: the exchange completed and reported no TF candidate.
const complete = (document, version = document.version) => ({
  uri: document.uri.toString(),
  version,
  state: 'complete',
  summary: 'no findings; TF: 0 candidates reported',
  detail: ['P1–P3 exact: no findings', 'TF: 0 candidates reported (test functions with a failure message)'],
});

test('shows the server status of the active Go document at its current version, with the detail as tooltip', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument();
  h.show(document);
  assert.match(h.item().text, /pending/);

  h.serverStatus(complete(document));

  assert.equal(h.item().visible, true);
  assert.match(h.item().text, /STAVE: no findings; TF: 0 candidates reported/);
  assert.match(h.item().tooltip, /test functions with a failure message/);
  assert.equal(h.item().command, 'stave.showOutput');
});

test('a status for another version is pending until the current version is reported', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument();
  h.show(document);
  h.serverStatus(complete(document));

  h.changed(document);
  assert.match(h.item().text, /pending/);
  h.serverStatus({ ...complete(document), state: 'unsaved', summary: 'buffer differs from the saved file (only the saved file is analysed)', detail: [] });
  assert.match(h.item().text, /buffer differs from the saved file/);
});

test('a late status for an older version does not overwrite the current completion', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument('file:///pkg/demo.go', 2);
  h.show(document);
  h.serverStatus(complete(document));

  h.serverStatus({ ...complete(document, 1), state: 'unsaved', summary: 'buffer differs from the saved file (only the saved file is analysed)', detail: [] });

  assert.match(h.item().text, /no findings; TF: 0 candidates reported/);
  assert.doesNotMatch(h.item().text, /pending|buffer differs/);
});

test('closing the document drops its status, so a reopened document at a repeated version starts pending', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const first = openDocument('file:///pkg/demo.go', 1);
  h.show(first);
  h.serverStatus(complete(first));
  h.closed(first);

  const second = openDocument('file:///pkg/demo.go', 1);
  h.show(second);

  assert.match(h.item().text, /pending/);
});

test('a status delivered while the document is closed is rejected, so the reopened document at that version stays pending', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const first = openDocument('file:///pkg/demo.go', 1);
  h.show(first);
  h.closed(first);

  h.serverStatus(complete(first));
  const second = openDocument('file:///pkg/demo.go', 1);
  h.show(second);

  assert.match(h.item().text, /pending/);
  assert.doesNotMatch(h.item().text, /no findings/);
});

test('a late status of a closed instance at a higher version does not block the reopened document', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const first = openDocument('file:///pkg/demo.go', 7);
  h.show(first);
  h.serverStatus(complete(first));
  h.closed(first);
  const second = openDocument('file:///pkg/demo.go', 1);
  h.show(second);

  h.serverStatus({ ...complete(first), summary: 'late: the closed instance completed' });
  assert.match(h.item().text, /pending/);
  h.serverStatus(complete(second));

  assert.match(h.item().text, /no findings; TF: 0 candidates reported/);
  assert.doesNotMatch(h.item().text, /pending|late/);
});

test('hides the item for a non-Go or non-file editor and shows it again for a Go file', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument();
  h.show(document);
  h.serverStatus(complete(document));

  h.show({ uri: { toString: () => 'untitled:Untitled-1', scheme: 'untitled' }, languageId: 'go', version: 1, isClosed: false });
  assert.equal(h.item().visible, false);
  h.show({ uri: { toString: () => 'file:///pkg/README.md', scheme: 'file' }, languageId: 'markdown', version: 1, isClosed: false });
  assert.equal(h.item().visible, false);
  h.show(undefined);
  assert.equal(h.item().visible, false);
  h.show(document);
  assert.equal(h.item().visible, true);
  assert.match(h.item().text, /no findings/);
});

test('replacing the server clears every status, and a status from the replaced server is ignored', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument();
  h.show(document);
  h.serverStatus(complete(document));
  const replaced = h.log.clients[0];

  h.fireConfigurationChange(['stave.serverArgs']);
  await waitFor(() => h.log.events.includes('started:1'), 'the replacement server to start');

  assert.doesNotMatch(h.item().text, /no findings/);
  assert.match(h.item().text, /pending/);
  h.serverStatus(complete(document), replaced);
  assert.match(h.item().text, /pending/);
  h.serverStatus(complete(document));
  assert.match(h.item().text, /no findings/);
});

// A crash can change client state without the extension explicitly stopping it.
test('a status delivered while the server is stopped never shows, and a restart starts pending', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument();
  h.show(document);
  h.serverStatus(complete(document));
  const [client] = h.log.clients;

  client.changeState(h.State.Running, h.State.Stopped);
  assert.match(h.item().text, /server not running/);
  h.serverStatus(complete(document));
  assert.match(h.item().text, /server not running/);

  client.changeState(h.State.Stopped, h.State.Starting);
  assert.match(h.item().text, /starting/);
  client.changeState(h.State.Starting, h.State.Running);
  assert.match(h.item().text, /pending/);
  assert.doesNotMatch(h.item().text, /no findings/);
});

test('a server that cannot be spawned shows not running and points at the output', async () => {
  const h = createHarness({ 'stave.serverPath': '/missing/stave' }, { failStart: [0] });
  await h.extension.activate(h.context);
  h.show(openDocument());

  assert.match(h.item().text, /server not running/);
  assert.match(h.item().tooltip, /STAVE output/);
  await h.vscode.commands.executeCommand('stave.showOutput');
  assert.ok(h.log.events.includes('show:channel:STAVE'));
});

test('after deactivation the item no longer claims a completed analysis', async () => {
  const h = createHarness();
  await h.extension.activate(h.context);
  const document = openDocument();
  h.show(document);
  h.serverStatus(complete(document));

  await h.extension.deactivate();

  assert.doesNotMatch(h.item().text, /no findings/);
});

test('activate resolves to a status API with a change event for the integration suite', async () => {
  const h = createHarness();
  const api = await h.extension.activate(h.context);
  const document = openDocument();
  let changes = 0;
  api.onDidChangeStatus(() => {
    changes += 1;
  });
  h.show(document);
  h.serverStatus(complete(document));

  assert.deepEqual(Object.keys(api.status()).sort(), ['state', 'text', 'tooltip', 'visible']);
  assert.equal(api.status().state, 'complete');
  assert.ok(changes >= 2);
});
