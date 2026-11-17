'use strict';

// "Report Issue…": opens a new GitHub issue with the versions filled in.
// It uses title + body query parameters rather than the issue forms' field
// ids, so it works even when the server isn't running and the forms change.
// Only versions and the OS go in the body: no paths, settings or workspace data.

const ISSUES_URL = 'https://github.com/qSharpy/azure-templates-navigator/issues';
// The feature guide: every feature with screenshots and shortcuts, for both IDEs.
const DOCS_URL = 'https://github.com/qSharpy/azure-templates-navigator/blob/main/docs/README.md';
// "What's New": the changelog shared by the server and both clients.
const CHANGELOG_URL = 'https://github.com/qSharpy/azure-templates-navigator/blob/main/CHANGELOG.md';
// "Give Feedback": the Marketplace listing's rating and review section.
const REVIEW_URL = 'https://marketplace.visualstudio.com/items?itemName=bogdanbujor.azure-templates-navigator&ssr=false#review-details';

// GitHub rejects very long URLs; stay well below the limit.
const MAX_URL = 7000;

const BODIES = {
  bug: {
    label: 'bug',
    sections: [
      ['What happened?', '<!-- What you did, what you saw, and what you expected instead. -->'],
      ['YAML that shows the problem', '<!-- The smallest pipeline and template that reproduce it. Remove anything private. -->\n\n```yaml\n\n```'],
      ['Log output', '<!-- Azure Templates: Show Log -->\n\n```text\n\n```'],
    ],
  },
  feature: {
    label: 'enhancement',
    sections: [
      ['What would you like?', '<!-- What you are trying to do, and what would make it easier. Feedback is welcome too. -->'],
    ],
  },
};

/**
 * @param {'bug'|'feature'} kind
 * @param {{ title?: string, environment: string[] }} info
 * @returns {string}
 */
function issueUrl(kind, { title = '', environment }) {
  const spec = BODIES[kind] || BODIES.bug;
  const body = spec.sections.map(([h, text]) => `### ${h}\n\n${text}\n`).join('\n')
    + `\n### Environment\n\n${environment.map(e => `- ${e}`).join('\n')}\n`;
  const query = new URLSearchParams({ labels: spec.label, title, body }).toString();
  const url = `${ISSUES_URL}/new?${query}`;
  return url.length <= MAX_URL ? url : `${ISSUES_URL}/new?${new URLSearchParams({ labels: spec.label, title })}`;
}

module.exports = { issueUrl, ISSUES_URL, DOCS_URL, CHANGELOG_URL, REVIEW_URL };
