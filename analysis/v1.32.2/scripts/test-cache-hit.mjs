// Cache hit experiment: send the same conversation twice, compare cache metrics.
import { readFileSync } from 'node:fs';

const env = Object.fromEntries(
  readFileSync('.env', 'utf-8')
    .split('\n')
    .filter(l => l && !l.startsWith('#') && l.includes('='))
    .map(l => [l.slice(0, l.indexOf('=')), l.slice(l.indexOf('=') + 1)])
);

const API = env.COMMAND_CODE_API_BASE || env.CC_API_BASE || 'https://api.commandcode.ai';
const KEY = env.COMMAND_CODE_API_KEY;
const SESSION_ID = crypto.randomUUID();

// Build a stable, reasonably large prefix so caching has something to chew on.
const bigSystem = 'You are a helpful coding assistant. ' +
  'Follow these rules strictly: ' +
  Array.from({ length: 40 }, (_, i) => `Rule ${i + 1}: always write clean, idiomatic, well-documented code in the language requested.`).join(' ');

function makeBody(round) {
  return {
    config: {
      workingDir: '/tmp/proxy-test',
      date: '2026-08-25',
      environment: 'linux',
      structure: [],
      isGitRepo: false,
      currentBranch: '',
      mainBranch: '',
      gitStatus: '',
      recentCommits: [],
    },
    memory: null,
    taste: null,
    skills: null,
    permissionMode: 'standard',
    mode: 'agent',
    params: {
      model: 'deepseek/deepseek-v4-flash',
      messages: [
        { role: 'user', content: [{ type: 'text', text: 'What is 2+2? Answer with just the number.' }] },
      ],
      system: bigSystem,
      max_tokens: 16,
      stream: true,
    },
  };
}

async function runRound(round) {
  const res = await fetch(`${API}/alpha/generate`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${KEY}`,
      'x-command-code-version': env.COMMAND_CODE_VERSION || env.CC_VERSION || '1.32.2',
      'x-cli-environment': 'production',
      'x-taste-learning': 'false',
      'x-session-id': SESSION_ID,          // same session both rounds
      'x-project-slug': 'd-users-dev-projects-cache-test-b7e1',
      'User-Agent': 'cli',
    },
    body: JSON.stringify(makeBody(round)),
  });

  if (!res.ok) {
    console.log(`Round ${round}: HTTP ${res.status}`, await res.text());
    return null;
  }

  const text = await res.text();
  let finishStep = null, finish = null;
  for (const line of text.split('\n')) {
    if (!line.trim()) continue;
    let ev;
    try { ev = JSON.parse(line); } catch { continue; }
    if (ev.type === 'finish-step') finishStep = ev;
    if (ev.type === 'finish') finish = ev;
  }

  const u = finish?.totalUsage ?? {};
  const ds = finishStep?.providerMetadata?.deepseek ?? {};
  const cost = finishStep?.providerMetadata?.gateway?.cost;
  return {
    round,
    inputTokens: u.inputTokens,
    cacheReadTokens: u.cachedInputTokens ?? u.inputTokenDetails?.cacheReadTokens,
    promptCacheHit: ds.promptCacheHitTokens,
    promptCacheMiss: ds.promptCacheMissTokens,
    outputTokens: u.outputTokens,
    cost,
  };
}

const r1 = await runRound(1);
console.log('Round 1:', JSON.stringify(r1));
await new Promise(r => setTimeout(r, 2000)); // small gap
const r2 = await runRound(2);
console.log('Round 2:', JSON.stringify(r2));

if (r1 && r2) {
  console.log('\n=== Verdict ===');
  console.log(`Round1 hit=${r1.promptCacheHit} miss=${r1.promptCacheMiss} cost=$${r1.cost}`);
  console.log(`Round2 hit=${r2.promptCacheHit} miss=${r2.promptCacheMiss} cost=$${r2.cost}`);
  console.log(r2.promptCacheHit > 0
    ? `CACHE HIT confirmed: ${r2.promptCacheHit}/${r2.inputTokens} tokens served from cache`
    : 'No cache hit on identical resend.');
}
