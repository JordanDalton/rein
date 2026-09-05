// Explicitly opt-in live validation. Uses the real Rein binary, MCP transport,
// policy/approval/audit APIs, and harmless system commands. No hosted inference.
import assert from 'node:assert/strict';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

const [profileDir, binary, phase = 'initial'] = process.argv.slice(2);
assert(profileDir && binary && path.isAbsolute(profileDir) && path.isAbsolute(binary),
  'Usage: node scripts/validate-live-mcp.mjs ABS_TEMP_PROFILE ABS_REIN_BINARY [initial|approved]');
assert(['initial', 'approved'].includes(phase));
assert(path.basename(profileDir).startsWith('rein-validation.'), 'Use a separate validation profile');
const profile = JSON.parse(await readFile(path.join(profileDir, 'cloud.json'), 'utf8'));
assert.equal(profile.control_url, 'https://reincontrol.com');
assert.equal(profile.organization, "Jordan Dalton's Team");
assert.equal(profile.device_name, 'Rein beta validation');
const agents = JSON.parse(await readFile(path.join(profileDir, 'agents.json'), 'utf8'));
assert(agents.agents.some(a => a.provider === 'rein-validation'), 'Register rein-validation first');

// Trusted, handwritten specs avoid discovery probes during validation.
await mkdir(path.join(profileDir, 'specs'), { recursive: true });
for (const tool of ['true', 'false', 'printf']) {
  await writeFile(path.join(profileDir, 'specs', `${tool}.json`), JSON.stringify({
    tool, binary: `/usr/bin/${tool}`, source: 'validation-fixture',
    root_help: `Validation-only system ${tool} executable.`, commands: [],
  }), { mode: 0o600 });
}

let plannedArgv = [];
let plannerCalls = 0;
const model = createServer(async (req, res) => {
  for await (const ignored of req) { /* Drain the local prompt; do not log it. */ }
  const plan = plannerCalls++ === 0
    ? { action: 'run', argv: plannedArgv, purpose: 'Rein live beta validation', risk: 'safe' }
    : { action: 'answer', answer: 'Validation command completed.' };
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({ choices: [{ message: { role: 'assistant', content: JSON.stringify(plan) } }] }));
});
await new Promise(resolve => model.listen(0, '127.0.0.1', resolve));
const child = spawn(binary, ['mcp', '--agent', 'rein-validation', '--backend', 'ollama',
  '--model', 'scripted-fixture', '--base-url', `http://127.0.0.1:${model.address().port}/v1`], {
  cwd: profileDir,
  env: { PATH: process.env.PATH, TMPDIR: process.env.TMPDIR || '/private/tmp', REIN_HOME: profileDir },
  stdio: ['pipe', 'pipe', 'pipe'],
});
child.stderr.on('data', chunk => process.stderr.write(chunk));
const pending = new Map();
let nextID = 1;
createInterface({ input: child.stdout }).on('line', line => {
  try {
    const reply = JSON.parse(line);
    if (pending.has(reply.id)) { pending.get(reply.id)(reply); pending.delete(reply.id); }
  } catch { /* Unexpected stdout will cause the bounded request to time out. */ }
});
function rpc(method, params) {
  const id = nextID++;
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => { pending.delete(id); reject(Error(`MCP ${method} timed out`)); }, 45000);
    pending.set(id, reply => { clearTimeout(timeout); reply.error ? reject(Error(JSON.stringify(reply.error))) : resolve(reply.result); });
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');
  });
}
async function run(tool, argv) {
  plannedArgv = argv;
  plannerCalls = 0;
  const result = await rpc('tools/call', { name: 'rein_in', arguments: {
    tool, intent: `Rein beta validation: ${tool}`, approval: 'safe',
  } });
  const text = result.content.filter(c => c.type === 'text').map(c => c.text).join('\n');
  console.log(`${tool}: ${text}`);
  return { result, text };
}
try {
  await rpc('initialize', { protocolVersion: '2025-11-25', capabilities: {}, clientInfo: { name: 'rein-live-validation', version: '1' } });
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
  if (phase === 'initial') {
    const allowed = await run('true', ['/usr/bin/true']);
    assert(!allowed.result.isError && allowed.text.includes('exit 0'), 'Allow path did not execute');
    const denied = await run('false', ['/usr/bin/false']);
    assert(denied.result.isError && denied.text.includes('blocked by policy') && !denied.text.includes('exit 1'), 'Deny path failed');
  }
  const approval = await run('printf', ['/usr/bin/printf', '%s\n', 'rein-beta-validation']);
  if (phase === 'initial') {
    assert(approval.text.includes('needs-approval') && !approval.text.includes('exit 0'), 'Approval gate failed');
    console.log('PASS: allow, deny, and pending approval. Await human approval before the approved phase.');
  } else {
    assert(!approval.result.isError && approval.text.includes('exit 0'), 'Approved command did not execute');
    console.log('PASS: human-approved command executed.');
  }
} finally {
  child.stdin.end();
  child.kill('SIGTERM');
  model.close();
}
