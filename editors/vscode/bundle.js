'use strict';

const fs = require('node:fs');
const path = require('node:path');
const esbuild = require('esbuild');

async function bundle(outdir = path.join(__dirname, 'out')) {
  const result = await esbuild.build({
    absWorkingDir: __dirname,
    entryPoints: ['extension.js'],
    bundle: true,
    platform: 'node',
    external: ['vscode'],
    outfile: path.join(outdir, 'extension.js'),
    metafile: true,
  });
  // The bundled language client resolves its Unix shutdown helper relative to extension.js.
  const helper = path.join(outdir, 'terminateProcess.sh');
  fs.copyFileSync(require.resolve('vscode-languageclient/lib/node/terminateProcess.sh'), helper);
  fs.chmodSync(helper, 0o755);
  return result;
}

if (require.main === module) {
  bundle().catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
}

module.exports = { bundle };
