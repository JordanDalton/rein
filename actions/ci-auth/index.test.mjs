import test from 'node:test';
import assert from 'node:assert/strict';
import { authenticate, mask } from './index.mjs';

const options = {
 controlUrl: 'https://rein.example',
 workloadId: '01ARZ3NDEKTSV4RRFFQ69G5FAV',
 audience: 'rein',
 env: { ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'runner-secret', ACTIONS_ID_TOKEN_REQUEST_URL: 'https://github.example/oidc?x=1' },
};
test('OIDC exchange uses explicit audience, no redirects, and separate credentials', async () => {
 const calls = [];
 const result = await authenticate({ ...options, request: async (url, init) => {
  calls.push([String(url), init]);
  return { ok: true, json: async () => calls.length === 1 ? { value: 'signed-assertion' } :
   { token: 'rein_wrk_example', expires_at: new Date(Date.now()+60000).toISOString() } };
 }});
 assert.equal(result.token, 'rein_wrk_example');
 assert.match(calls[0][0], /audience=rein/);
 assert.equal(calls[0][1].headers.Authorization, 'Bearer runner-secret');
 assert.equal(calls[1][1].headers.Authorization, undefined);
 assert.equal(JSON.parse(calls[1][1].body).assertion, 'signed-assertion');
 assert.ok(calls.every(([,init]) => init.redirect === 'error'));
});
test('rejects insecure destinations, missing OIDC permission, and HTTP failures', async () => {
 await assert.rejects(authenticate({ ...options, controlUrl: 'http://rein.example' }));
 await assert.rejects(authenticate({ ...options, env: {} }));
 await assert.rejects(authenticate({ ...options, request: async () => ({ ok: false, status: 401 }) }));
});
test('rejects token newline injection', async () => {
 let count = 0;
 await assert.rejects(authenticate({ ...options, request: async () => ({
  ok: true, json: async () => ++count === 1 ? { value: 'assertion' } :
   { token: 'rein_wrk_test\nINJECT=value', expires_at: new Date(Date.now()+60000).toISOString() },
 }) }));
 assert.equal(mask('a%\nb\r'), 'a%25%0Ab%0D');
});
