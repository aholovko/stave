'use strict';

const vscode = require('vscode');
const { LanguageClient, State } = require('vscode-languageclient/node');

// The extension owns the shared channel so it survives restarts and failed starts.
let output;

// The session owns the client and watcher so stop can release both; LanguageClient does not dispose supplied watchers.
let session;

// Serialize lifecycle changes: a starting client cannot be stopped, and servers must not overlap.
let lifecycle = Promise.resolve();

// Empty diagnostics cannot distinguish completion from missing coverage. Status is valid only for the current
// document version and server. URI/version alone cannot distinguish a reopened file at the same version.
const statusMethod = 'stave/status';
const showOutputCommand = 'stave.showOutput';
const icons = {
  running: '$(sync~spin)',
  complete: '$(check)',
  unsaved: '$(pencil)',
  'not-analysed': '$(circle-slash)',
  failed: '$(warning)',
};
let statusBar;
let statusChanged;
const statuses = new Map(); // uri -> the last status the running server sent for it
let serverState = 'stopped'; // starting | running | stopped
let shown = { visible: false, state: 'hidden', text: '', tooltip: '' };

function activate(context) {
  output = vscode.window.createOutputChannel('STAVE');
  statusChanged = new vscode.EventEmitter();
  statusBar = vscode.window.createStatusBarItem('stave.status', vscode.StatusBarAlignment.Left, 0);
  statusBar.name = 'STAVE analysis status';
  statusBar.command = showOutputCommand;
  context.subscriptions.push(
    statusBar,
    statusChanged,
    vscode.commands.registerCommand(showOutputCommand, () => output.show(true)),
    vscode.window.onDidChangeActiveTextEditor(render),
    vscode.workspace.onDidChangeTextDocument(render),
    vscode.workspace.onDidCloseTextDocument((document) => {
      statuses.delete(document.uri.toString());
      render();
    }),
    vscode.workspace.onDidChangeConfiguration((event) => {
      if (event.affectsConfiguration('stave.serverPath') || event.affectsConfiguration('stave.serverArgs')) {
        enqueue(stop);
        enqueue(start);
      }
    })
  );
  render();
  // The integration suite reads what the item shows through this API rather than through the status bar.
  return enqueue(start).then(() => ({ status: () => ({ ...shown }), onDidChangeStatus: statusChanged.event }));
}

function deactivate() {
  return enqueue(stop).then(() => output.dispose());
}

function enqueue(step) {
  lifecycle = lifecycle.then(step).catch(report);
  return lifecycle;
}

// Keep the queue usable after failure so correcting settings can recover. Client rejections may be strings.
function report(error) {
  output.appendLine(`STAVE extension: ${error instanceof Error ? error.stack : error}`);
}

function start() {
  const config = vscode.workspace.getConfiguration('stave');
  // Omitting transport uses stdio; TransportKind.stdio would append another --stdio argument.
  const server = { command: config.get('serverPath'), args: config.get('serverArgs') };
  const watcher = vscode.workspace.createFileSystemWatcher('**/*.go');
  const client = new LanguageClient('stave', 'STAVE', server, {
    documentSelector: [{ scheme: 'file', language: 'go' }],
    synchronize: { fileEvents: watcher },
    outputChannel: output,
  });
  // Register before start to catch initial statuses. Reject future versions: they can belong to a previously closed
  // instance and would otherwise block all statuses for the reopened document.
  client.onNotification(statusMethod, (status) => {
    if (session?.client !== client) {
      return;
    }
    const document = vscode.workspace.textDocuments.find((open) => open.uri.toString() === status.uri);
    if (document === undefined || status.version > document.version) {
      return;
    }
    const current = statuses.get(status.uri);
    if (current !== undefined && current.version > status.version) {
      return;
    }
    statuses.set(status.uri, status);
    render();
  });
  client.onDidChangeState(({ newState }) => {
    if (session?.client !== client) {
      return;
    }
    serverState = newState === State.Running ? 'running' : newState === State.Starting ? 'starting' : 'stopped';
    if (newState !== State.Running) {
      statuses.clear();
    }
    render();
  });
  session = { client, watcher };
  return client.start();
}

async function stop() {
  const { client, watcher } = session;
  session = undefined;
  statuses.clear();
  serverState = 'stopped';
  render();
  try {
    // The client can restart itself after a crash, outside our lifecycle queue.
    if (client.state === State.Starting) {
      await client.start().catch(report);
    }
    // A client whose start failed is not running, and stopping it throws.
    if (client.needsStop()) {
      await client.stop();
    }
  } finally {
    watcher.dispose();
  }
}

function describe(document) {
  if (serverState === 'starting') {
    return {
      state: 'starting',
      text: '$(sync~spin) STAVE: starting',
      tooltip: 'The STAVE language server is starting. Click to open the STAVE output.',
    };
  }
  if (serverState !== 'running') {
    return {
      state: 'stopped',
      text: '$(warning) STAVE: server not running',
      tooltip: 'The STAVE language server is not running. Click to open the STAVE output.',
    };
  }
  const status = statuses.get(document.uri.toString());
  if (status === undefined || status.version !== document.version) {
    return {
      state: 'pending',
      text: '$(sync~spin) STAVE: pending',
      tooltip: 'This version of the document has not been analysed yet. Click to open the STAVE output.',
    };
  }
  const detail = Array.isArray(status.detail) ? status.detail : [];
  return {
    state: status.state,
    text: `${icons[status.state] ?? '$(info)'} STAVE: ${status.summary}`,
    tooltip: [...detail, 'Click to open the STAVE output.'].join('\n'),
  };
}

function render() {
  const document = vscode.window.activeTextEditor?.document;
  if (document === undefined || document.languageId !== 'go' || document.uri.scheme !== 'file') {
    statusBar.hide();
    shown = { visible: false, state: 'hidden', text: '', tooltip: '' };
  } else {
    const view = describe(document);
    statusBar.text = view.text;
    statusBar.tooltip = view.tooltip;
    statusBar.show();
    shown = { visible: true, ...view };
  }
  statusChanged.fire(shown);
}

module.exports = { activate, deactivate };
