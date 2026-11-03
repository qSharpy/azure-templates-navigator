'use strict';
// End-to-end test: launches a real VS Code with this extension and the
// repository samples. Run: node test/e2e/run.js
const path = require('path');
const { runTests } = require('@vscode/test-electron');

(async () => {
  try {
    await runTests({
      extensionDevelopmentPath: path.resolve(__dirname, '../..'),
      extensionTestsPath: path.resolve(__dirname, 'suite.js'),
      launchArgs: [path.resolve(__dirname, '../../../../samples'), '--disable-extensions',
        '--user-data-dir=' + require('fs').mkdtempSync(require('path').join(require('os').tmpdir(), 'atn-'))],
    });
  } catch (err) {
    console.error('E2E failed', err);
    process.exit(1);
  }
})();
