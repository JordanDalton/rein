import { appendFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

function httpsUrl(value, originOnly = false) {
  const url = new URL(value);
  if (url.protocol !== 'https:' || url.username || url.password || url.hash ||
      (originOnly && (url.search || (url.pathname !== '/' && url.pathname !== '')))) {
    throw new Error('Expected a trusted HTTPS origin');
  }
  return url;
}

export async function authenticate({ controlUrl, workloadId, audience, env, request = fetch }) {
  const control = httpsUrl(controlUrl, true).origin;
  if (!/^[0-9A-HJKMNP-TV-Z]{26}$/i.test(workloadId)) throw new Error('Invalid workload ID');
  if (!audience || /[\r\n]/.test(audience)) throw new Error('An explicit audience is required');
  if (!env.ACTIONS_ID_TOKEN_REQUEST_TOKEN || !env.ACTIONS_ID_TOKEN_REQUEST_URL) {
    throw new Error('GitHub OIDC unavailable: grant this job id-token: write');
  }
  const oidcUrl = httpsUrl(env.ACTIONS_ID_TOKEN_REQUEST_URL);
  oidcUrl.searchParams.set('audience', audience);
  async function json(url, options) {
    const response = await request(url, { ...options, redirect: 'error', signal: AbortSignal.timeout(15000) });
    if (!response.ok) throw new Error('CI authentication request failed (HTTP ' + response.status + ')');
    return response.json();
  }
  const assertion = await json(oidcUrl, { headers: { Authorization: 'Bearer ' + env.ACTIONS_ID_TOKEN_REQUEST_TOKEN } });
  if (typeof assertion.value !== 'string' || assertion.value.length > 16384) throw new Error('Invalid OIDC response');
  const result = await json(control + '/api/v1/rein/workloads/' + workloadId + '/federate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ assertion: assertion.value }),
  });
  if (typeof result.token !== 'string' || !/^rein_wrk_[A-Za-z0-9]+$/.test(result.token) ||
      typeof result.expires_at !== 'string' || /[\r\n]/.test(result.expires_at) ||
      !(Date.parse(result.expires_at) > Date.now())) throw new Error('Invalid workload credential response');
  return { token: result.token, expiresAt: result.expires_at, control };
}

export function mask(value) {
  return value.replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    if (!process.env.GITHUB_ENV || !process.env.GITHUB_OUTPUT) throw new Error('GitHub runner environment unavailable');
    const result = await authenticate({
      controlUrl: process.env.INPUT_CONTROL_URL,
      workloadId: process.env.INPUT_WORKLOAD_ID,
      audience: process.env.INPUT_AUDIENCE,
      env: process.env,
    });
    console.log('::add-mask::' + mask(result.token));
    appendFileSync(process.env.GITHUB_ENV, 'REIN_WORKLOAD_TOKEN=' + result.token + '\nREIN_CONTROL_URL=' + result.control + '\n');
    appendFileSync(process.env.GITHUB_OUTPUT, 'expires-at=' + result.expiresAt + '\n');
    console.log('Rein workload authenticated. Run rein ci check before execution. Credential expires at ' + result.expiresAt);
  } catch {
    // Never print response bodies, request URLs, assertions, or exception stacks.
    console.error('Rein CI authentication failed. Check workload trust, audience, endpoint, and job id-token permission.');
    process.exitCode = 1;
  }
}
