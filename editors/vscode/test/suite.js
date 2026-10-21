'use strict';

// Exercise real editor/server integration without asserting model predictions. Require explicit completion, candidate
// coverage and valid source positions. Timings include each backend process start and model load.

const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const vscode = require('vscode');

const { eventually } = require('./eventually.js');

const severities = ['Error', 'Warning', 'Information', 'Hint'];
const codes = /^(P[1-3]|TF|DC|RP|coverage)$/;
// The classes each file has candidates for; the backend must report a positive count for each.
const judged = {
  'demo.go': ['P1', 'P2', 'P3', 'DC', 'RP'],
  'demo_test.go': ['TF'],
  'external_test.go': ['TF'],
  'incomplete_test.go': ['TF'],
};

function span({ start, end }) {
  return `${start.line}:${start.character}-${end.line}:${end.character}`;
}

function backendLine(id) {
  return new RegExp(`^backend ${id.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')} \\(`, 'm');
}

// Recompute byte spans independently from editor positions to catch encoding errors.
function observe(document) {
  const text = document.getText();
  const saved = fs.readFileSync(document.uri.fsPath);
  const bytes = (position) => Buffer.byteLength(text.slice(0, document.offsetAt(position)), 'utf8');
  return vscode.languages.getDiagnostics(document.uri).map((diagnostic) => {
    const code = diagnostic.code !== null && typeof diagnostic.code === 'object' ? diagnostic.code.value : diagnostic.code;
    assert.ok(codes.test(String(code)), `unexpected diagnostic code ${JSON.stringify(code)}`);
    assert.ok(document.validateRange(diagnostic.range).isEqual(diagnostic.range), `${span(diagnostic.range)} lies outside`);
    const range = [bytes(diagnostic.range.start), bytes(diagnostic.range.end)];
    const data = diagnostic.data;
    if (data?.span) {
      assert.deepEqual(range, [data.span.start, data.span.end], `${code} positions do not map back to its byte span`);
      assert.equal(data.source_sha256, crypto.createHash('sha256').update(saved).digest('hex'));
    }
    return {
      code,
      origin: data?.origin ?? null,
      severity: severities[diagnostic.severity],
      range: span(diagnostic.range),
      bytes: range,
      text: saved.subarray(range[0], range[1]).toString('utf8'),
      message: diagnostic.message,
      data: data ?? null,
    };
  });
}

async function scenario(context, step, shared) {
  const deadline = { settleMs: 1000, deadlineMs: context.deadlineMs };
  const extension = vscode.extensions.getExtension('stave.stave-vscode');
  let api;

  // Measure the first matching completion, then wait for stability to catch late stale results. The target must be
  // the active document; a failed status rejects immediately.
  const finished = async (target, what, trigger, extra = () => {}, backend = context.backend) => {
    let first;
    let fail;
    const failed = new Promise((_, reject) => {
      fail = reject;
    });
    failed.catch(() => {});
    const check = () => {
      assert.ok(target() !== undefined && vscode.window.activeTextEditor?.document === target(), 'target not active');
      const status = api.status();
      assert.equal(status.state, 'complete', `state ${status.state}: ${status.text}\n${status.tooltip}`);
      assert.match(status.tooltip, backendLine(backend), `no backend ${backend} in:\n${status.tooltip}`);
      extra(status);
    };
    const record = () => {
      const status = api.status();
      if (target() !== undefined && vscode.window.activeTextEditor?.document === target() && status.state === 'failed') {
        fail(new Error(`${what}: the analysis failed: ${status.text}\n${status.tooltip}`));
      }
      try {
        check();
      } catch {
        return;
      }
      first ??= { ms: Date.now() - started, status };
    };
    const started = Date.now();
    const recording = api.onDidChangeStatus(record);
    try {
      await trigger();
      record();
      await Promise.race([eventually(api.onDidChangeStatus, check, what, deadline), failed]);
    } finally {
      recording.dispose();
    }
    return first;
  };

  await step('activate the extension', async () => {
    assert.ok(extension, 'the extension was not loaded');
    if (context.extensionsDir !== null) {
      const relative = path.relative(context.extensionsDir, extension.extensionPath);
      assert.ok(
        relative !== '' && !relative.startsWith('..') && !path.isAbsolute(relative),
        `the extension was loaded from ${extension.extensionPath}, not installed in ${context.extensionsDir}`
      );
    }
    api = await extension.activate();
    shared.api = api;
    return `stave-vscode ${extension.packageJSON.version} from ${extension.extensionPath} in VS Code ${vscode.version}`;
  });

  const documents = shared.documents;
  for (const file of context.files) {
    const name = path.basename(file);
    await step(`open and save ${name}: a complete ${context.backend} status, candidates judged and valid positions`, async () => {
      const opened = async () => {
        documents[name] = await vscode.workspace.openTextDocument(vscode.Uri.file(file));
        await vscode.window.showTextDocument(documents[name]);
        await vscode.commands.executeCommand('workbench.action.files.save');
      };
      const counted = (status) => {
        for (const id of judged[name]) {
          const count = new RegExp(`^${id}: [1-9]\\d* candidates? reported`, 'm');
          assert.match(status.tooltip, count, `no ${id} candidates in:\n${status.tooltip}`);
        }
      };
      const first = await finished(() => documents[name], `a complete status for ${name}`, opened, counted);
      return { trigger: 'open, show and save', first, reported: observe(documents[name]) };
    });
  }

  const demo = documents['demo.go'];
  const original = fs.readFileSync(demo.uri.fsPath, 'utf8');
  for (let cycle = 1; cycle <= context.cycles; cycle++) {
    await step(`cycle ${cycle}: dirty clears, restoring the saved bytes reanalyses`, async () => {
      await vscode.window.showTextDocument(demo);
      const dirty = new vscode.WorkspaceEdit();
      dirty.insert(demo.uri, new vscode.Position(0, 0), '// unsaved\n');
      assert.ok(await vscode.workspace.applyEdit(dirty));
      await eventually(api.onDidChangeStatus, () => assert.equal(api.status().state, 'unsaved'), 'unsaved', deadline);
      assert.deepEqual(vscode.languages.getDiagnostics(demo.uri), [], 'a dirty buffer keeps diagnostics');

      const restore = new vscode.WorkspaceEdit();
      restore.replace(demo.uri, new vscode.Range(new vscode.Position(0, 0), demo.positionAt(demo.getText().length)), original);
      // Restoring the saved text triggers analysis before the following save, which is deduplicated.
      const first = await finished(() => demo, `cycle ${cycle}: a complete status after the restore`, async () => {
        assert.ok(await vscode.workspace.applyEdit(restore));
        assert.ok(await demo.save());
      });
      assert.equal(fs.readFileSync(demo.uri.fsPath, 'utf8'), original, 'the saved bytes are not the original');
      return { trigger: 'restore edit, then save', version: demo.version, first, reported: observe(demo) };
    });
  }

  await step('a saved sibling change reanalyses the open test file, and restoring it does again', async () => {
    const test = documents['demo_test.go'];
    await vscode.window.showTextDocument(test);
    const contextOf = (status) => /^saved source \S+, context (\S+)$/m.exec(status.tooltip ?? '')?.[1];
    const before = contextOf(api.status());
    assert.ok(before, `no context digest in ${JSON.stringify(api.status().tooltip)}`);
    const results = [];
    for (const [bytes, changed] of [[`${original}// comment-only change\n`, true], [original, false]]) {
      const first = await finished(
        () => test,
        'the test file reanalysed under the new context',
        async () => fs.writeFileSync(demo.uri.fsPath, bytes),
        (status) => {
          assert.equal(contextOf(status) !== before, changed, `context ${contextOf(status)} after the sibling write`);
        }
      );
      results.push({ trigger: changed ? 'sibling comment written' : 'sibling restored', first, reported: observe(test) });
    }
    assert.equal(fs.readFileSync(demo.uri.fsPath, 'utf8'), original);
    return results;
  });

  if (context.switchTo === null) {
    return;
  }
  await step(`stave model use ${context.switchTo}: the next unchanged save names it`, async () => {
    await vscode.window.showTextDocument(demo);
    const first = await finished(
      () => demo,
      `a complete status naming ${context.switchTo} after the switch`,
      async () => {
        const used = spawnSync(context.server, ['model', 'use', context.switchTo], {
          env: { ...process.env, STAVE_HOME: context.home },
          encoding: 'utf8',
        });
        assert.equal(used.status, 0, `stave model use ${context.switchTo}: ${used.error?.message ?? used.stderr}`);
        assert.equal(demo.isDirty, false, 'the save would not be unchanged');
        await vscode.commands.executeCommand('workbench.action.files.save');
      },
      () => {},
      context.switchTo
    );
    assert.equal(fs.readFileSync(demo.uri.fsPath, 'utf8'), original, 'the saved bytes are not the original');
    return { trigger: 'stave model use, then an unchanged save', first, reported: observe(demo) };
  });
}

async function run() {
  const workspace = vscode.workspace.workspaceFolders[0].uri.fsPath;
  const context = JSON.parse(fs.readFileSync(path.join(workspace, '..', 'context.json'), 'utf8'));
  const receipt = { ok: false, steps: [] };
  const save = () => fs.writeFileSync(context.receipt, `${JSON.stringify(receipt, null, 2)}\n`);
  const step = async (name, body) => {
    const entry = { name, ok: false, ms: 0 };
    const started = Date.now();
    receipt.steps.push(entry);
    try {
      entry.detail = await body();
      entry.ok = true;
    } finally {
      entry.ms = Date.now() - started;
      save();
    }
  };
  const shared = { api: undefined, documents: {} };
  try {
    await scenario(context, step, shared);
    receipt.ok = true;
  } catch (error) {
    receipt.error = { message: error.message, stack: error.stack };
    // Diagnostic collection must not mask the original failure.
    try {
      receipt.lastStatus = shared.api?.status() ?? null;
      receipt.active = vscode.window.activeTextEditor?.document.uri.fsPath ?? null;
      receipt.reported = Object.fromEntries(
        Object.entries(shared.documents).map(([name, document]) => [
          name,
          vscode.languages.getDiagnostics(document.uri).map((d) => ({ code: d.code, range: span(d.range), message: d.message })),
        ])
      );
    } catch (unavailable) {
      receipt.reported = `unavailable: ${unavailable.message}`;
    }
    throw error;
  } finally {
    save();
  }
}

module.exports = { run };
