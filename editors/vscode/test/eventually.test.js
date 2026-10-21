'use strict';

// Mock time to test settling and deadlines without real delays.

const assert = require('node:assert/strict');
const { test } = require('node:test');

const { eventually } = require('./eventually.js');

// A change source in the shape of a VS Code event: subscribing returns a disposable.
function changeSource() {
  const listeners = new Set();
  return {
    listeners,
    subscribe(listener) {
      listeners.add(listener);
      return { dispose: () => listeners.delete(listener) };
    },
    fire() {
      for (const listener of [...listeners]) {
        listener();
      }
    },
  };
}

// Observe settlement without awaiting the promise under test.
function track(promise) {
  const outcome = { settled: false };
  promise.then(
    () => Object.assign(outcome, { settled: true, resolved: true }),
    (error) => Object.assign(outcome, { settled: true, resolved: false, error })
  );
  return outcome;
}

const flush = () => new Promise((resolve) => setImmediate(resolve));
const windows = { settleMs: 100, deadlineMs: 1000 };

test('resolves only after the check has held for the whole settle window', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const source = changeSource();
  let state = 'pending';
  const outcome = track(eventually(source.subscribe, () => assert.equal(state, 'ready'), 'readiness', windows));

  state = 'ready';
  source.fire();
  t.mock.timers.tick(99);
  await flush();
  assert.equal(outcome.settled, false);
  t.mock.timers.tick(1);
  await flush();

  assert.equal(outcome.resolved, true);
  assert.equal(source.listeners.size, 0, 'the subscription was not disposed');
});

test('a state that already holds is still watched for the settle window', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const source = changeSource();
  let state = 'ready';
  const outcome = track(eventually(source.subscribe, () => assert.equal(state, 'ready'), 'readiness', windows));

  t.mock.timers.tick(60);
  state = 'stale result published late';
  source.fire();
  t.mock.timers.tick(500);
  await flush();

  assert.equal(outcome.settled, false);
});

test('a change that breaks the check cancels the pending success until the state holds again', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const source = changeSource();
  let state = 'ready';
  const outcome = track(eventually(source.subscribe, () => assert.equal(state, 'ready'), 'readiness', windows));

  t.mock.timers.tick(50);
  state = 'flicker';
  source.fire();
  t.mock.timers.tick(50);
  state = 'ready';
  source.fire();
  t.mock.timers.tick(99);
  await flush();
  assert.equal(outcome.settled, false);
  t.mock.timers.tick(1);
  await flush();

  assert.equal(outcome.resolved, true);
});

test('rejects at the deadline with what was awaited and the last failure, and lets go of the source', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const source = changeSource();
  const outcome = track(
    eventually(source.subscribe, () => assert.fail('P3 is still reported'), 'P3 to disappear', windows)
  );

  source.fire();
  t.mock.timers.tick(1000);
  await flush();

  assert.equal(outcome.resolved, false);
  assert.match(outcome.error.message, /P3 to disappear/);
  assert.match(outcome.error.message, /1000 ms/);
  assert.match(outcome.error.message, /P3 is still reported/);
  assert.equal(source.listeners.size, 0, 'the subscription was not disposed');
});

test('a late pass cannot resolve a wait the deadline has already rejected', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const source = changeSource();
  let state = 'pending';
  const outcome = track(eventually(source.subscribe, () => assert.equal(state, 'ready'), 'readiness', windows));

  t.mock.timers.tick(950);
  state = 'ready';
  source.fire();
  t.mock.timers.tick(200);
  await flush();

  assert.equal(outcome.resolved, false);
  assert.match(outcome.error.message, /readiness/);
});
