'use strict';

// subscribe returns a disposable and announces changes; check throws until the desired state holds.
// Require settleMs without changes to catch late stale results. Reject after deadlineMs with the last failure.
function eventually(subscribe, check, what, { settleMs, deadlineMs }) {
  return new Promise((resolve, reject) => {
    let failure;
    let settling;
    const finish = (settle, value) => {
      clearTimeout(deadline);
      clearTimeout(settling);
      subscription.dispose();
      settle(value);
    };
    const evaluate = () => {
      clearTimeout(settling);
      try {
        check();
      } catch (error) {
        failure = error;
        return;
      }
      settling = setTimeout(() => finish(resolve), settleMs);
    };
    const deadline = setTimeout(() => {
      const reason = failure ? failure.message : `it did not stay unchanged for ${settleMs} ms`;
      finish(reject, new Error(`${what}: not reached within ${deadlineMs} ms: ${reason}`));
    }, deadlineMs);
    const subscription = subscribe(evaluate);
    evaluate();
  });
}

module.exports = { eventually };
