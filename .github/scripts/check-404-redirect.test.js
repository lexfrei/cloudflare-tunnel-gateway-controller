// check-404-redirect.test.js — unit tests for the versionless-link redirect
// shipped inline in docs/404.html.
//
// The docs site is versioned with mike: only /latest/<page>/ (and /vX.Y/<page>/)
// resolve, so a versionless https://cf.k8s.lex.la/<page>/ hits GitHub Pages'
// fallback. docs/404.html, served at the gh-pages root, JS-redirects such paths
// to /latest/. These tests pin that behaviour.
//
// Runs under stdlib `node --test`; no third-party test framework so the CI step
// doesn't have to install npm deps.
//
// The test extracts and executes the REAL inline <script> from docs/404.html
// (not a copy) so it can't drift from the file GitHub Pages actually serves.

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(
  path.join(__dirname, '..', '..', 'docs', '404.html'),
  'utf8',
);

const scriptMatch = html.match(/<script>([\s\S]*?)<\/script>/);
assert.ok(scriptMatch, 'docs/404.html must contain an inline <script> redirect');
const scriptSource = scriptMatch[1];

// Execute the inline script against a stubbed window.location for the given URL.
// Returns the argument passed to location.replace(), or null if no redirect.
function redirectFor(pathname, search = '', hash = '') {
  let redirectedTo = null;
  const location = {
    pathname,
    search,
    hash,
    replace(target) {
      redirectedTo = target;
    },
  };
  const sandbox = { window: { location } };
  vm.createContext(sandbox);
  vm.runInContext(scriptSource, sandbox);
  return redirectedTo;
}

test('redirects a versionless doc path to /latest/', () => {
  assert.equal(
    redirectFor('/gateway-api/limitations/'),
    '/latest/gateway-api/limitations/',
  );
});

test('preserves the hash fragment', () => {
  assert.equal(
    redirectFor('/reference/security/', '', '#rbac-configuration'),
    '/latest/reference/security/#rbac-configuration',
  );
});

test('preserves the query string', () => {
  assert.equal(
    redirectFor('/operations/metrics/', '?q=test'),
    '/latest/operations/metrics/?q=test',
  );
});

test('redirects a section whose name starts with "dev" (development != dev)', () => {
  // Guards against a prefix-match bug: the version allowlist must match the
  // whole first segment, not a prefix, or /development/ would be mistaken for
  // the /dev/ alias and never redirect.
  assert.equal(
    redirectFor('/development/architecture/'),
    '/latest/development/architecture/',
  );
});

test('does not redirect a path already under /latest/ (loop-safe)', () => {
  assert.equal(redirectFor('/latest/gateway-api/limitations/'), null);
});

test('does not redirect a semver-versioned path', () => {
  assert.equal(redirectFor('/3.0.0/gateway-api/limitations/'), null);
});

test('does not redirect a v-prefixed versioned path', () => {
  assert.equal(redirectFor('/v3.0/gateway-api/limitations/'), null);
});

test('does not redirect the dev alias', () => {
  assert.equal(redirectFor('/dev/index.html'), null);
});

test('does not redirect site assets', () => {
  assert.equal(redirectFor('/assets/stylesheets/main.css'), null);
});

test('does not redirect the bare root', () => {
  assert.equal(redirectFor('/'), null);
});
