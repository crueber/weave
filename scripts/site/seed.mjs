// Seeds fictional data for the website screenshots over the REST API (called by seed.sh). Every name is invented.
// Boards that the site says "start from a template" are created from that built-in template (board.instantiate).
import fs from 'fs';

const BASE = process.env.WEAVE_BASE_URL;
const DIR = process.env.DEMO_DIR;
const keys = JSON.parse(fs.readFileSync(DIR + '/keys.json', 'utf8'));
const sess = JSON.parse(fs.readFileSync(DIR + '/sessions.json', 'utf8'));

async function api(who, method, path, body) {
  const key = keys[who] || who; // a user name from keys.json, or a raw key (the agent)
  const r = await fetch(BASE + '/api/v1' + path, {
    method, headers: {Authorization: 'Bearer ' + key, 'Content-Type': 'application/json'},
    body: body ? JSON.stringify(body) : undefined,
  });
  const t = await r.text();
  const j = t ? JSON.parse(t) : {};
  if (!r.ok) throw new Error(`${method} ${path}: ${t}`);
  return j.item ?? j.items ?? j;
}
const A = (m, p, b) => api('avery', m, p, b);
const at = d => d.toISOString().slice(0, 10) + 'T17:00';
const day = n => at(new Date(Date.now() + n * 864e5));
// the timeline is dated from the first of NEXT month (shots open the calendar on that month, so it is full and
// nothing upcoming is overdue); the two finished cards are dated a few days ago
const som = new Date(); som.setUTCDate(1); som.setUTCMonth(som.getUTCMonth() + 1);
const dom = n => at(new Date(som.getTime() + (n - 1) * 864e5));
const timelineMonth = som.toISOString().slice(0, 7);

async function boardParts(boardId) {
  const lists = {}; for (const l of await A('GET', `/boards/${boardId}/lists`)) lists[l.name] = l.id;
  const labels = {}; for (const l of await A('GET', `/boards/${boardId}/labels`)) labels[l.name] = l.id;
  return {lists, labels};
}
async function members(boardId, who) { for (const u of who) await A('POST', `/boards/${boardId}/members`, {userId: sess[u].id, role: 'editor'}); }

function maker(P) {
  return async function card(list, name, o = {}) {
    const c = await A('POST', `/lists/${P.lists[list]}/cards`, {name, description: o.desc, dueDate: o.due});
    for (const l of o.labels || []) await A('POST', `/cards/${c.id}/labels`, {labelId: P.labels[l]});
    for (const m of o.members || []) await A('POST', `/cards/${c.id}/members`, {userId: sess[m].id});
    if (o.tasks) {
      const tl = await A('POST', `/cards/${c.id}/task-lists`, {name: o.tasks.name || 'Checklist', showOnFrontOfCard: true});
      for (const [t, done] of o.tasks.items) await A('POST', `/task-lists/${tl.id}/tasks`, {name: t, isCompleted: !!done});
    }
    for (const [who, text] of o.comments || []) await api(who, 'POST', `/cards/${c.id}/comments`, {text});
    if (o.done) await A('PATCH', `/cards/${c.id}`, {isDueCompleted: true}).catch(() => {});
    return c;
  };
}

// ---------------------------------------------------------------- personal: My week (Weekly planner template)
const personal = await A('POST', '/projects', {name: 'Personal', description: 'Home, health and the week ahead.'});
const week = await A('POST', `/projects/${personal.id}/boards/from-template`, {templateId: 'builtin:weekly-planner', name: 'My week', sampleCards: false});
const W = await boardParts(week.id);
const wc = maker(W);
await wc('Mon', 'Gym before work', {labels: ['Health']});
await wc('Mon', 'Plan the sprint review', {labels: ['Work']});
await wc('Tue', 'Dentist at 9:30', {labels: ['Health'], due: day(3)});
await wc('Tue', 'Review the Q4 budget', {labels: ['Work']});
await wc('Wed', 'Groceries', {labels: ['Home'], tasks: {name: 'Shopping list', items: [['Oats', 1], ['Coffee', 1], ['Lemons', 0], ['Rice', 0]]}});
await wc('Wed', 'Call the plumber', {labels: ['Home']});
await wc('Thu', 'Book flights for the holidays', {labels: ['Home'], due: day(5)});
await wc('Thu', 'Write the onboarding guide', {labels: ['Work']});
await wc('Fri', 'Pizza night with friends', {labels: ['Home']});
await wc('Fri', 'Yoga class', {labels: ['Health']});
await wc('Weekend', 'Long run, 10 km', {labels: ['Health']});
await wc('Weekend', 'Fix the bike brakes', {labels: ['Home']});
await wc('Someday', 'Learn to bake bread');
await wc('Someday', 'Repaint the hallway', {labels: ['Home']});

// ---------------------------------------------------------------- Apollo: the software project
const apollo = await A('POST', '/projects', {name: 'Apollo', description: 'Apollo 2.0: the platform work, the road to launch and the bugs on the way.'});

// Apollo Platform, from the Sprint (Scrum) template
const plat = await A('POST', `/projects/${apollo.id}/boards/from-template`, {templateId: 'builtin:sprint', name: 'Apollo Platform', sampleCards: false});
const P = await boardParts(plat.id);
const pc = maker(P);
await members(plat.id, ['jordan', 'sam', 'riley']);
await A('PATCH', `/lists/${P.lists['Sprint']}`, {description: 'Ready to pick up. Each card says what done means; ask in a comment when it does not.'});
await pc('Backlog', 'Rate-limit the public API', {labels: ['Feature'], members: ['sam']});
await pc('Backlog', 'Spike: rank search results by recency', {labels: ['Spike']});
await pc('Backlog', 'Remove the legacy v1 endpoints', {labels: ['Chore'], members: ['jordan']});
await pc('Backlog', 'Flaky upload test on CI', {labels: ['Bug']});
await pc('Sprint', 'Webhook retries with backoff', {labels: ['Feature'], members: ['jordan'], due: day(6)});
await pc('Sprint', 'Fix timezone in digest emails', {labels: ['Bug'], members: ['sam'], due: day(2)});
await pc('Sprint', 'Update dependencies', {labels: ['Chore']});
await pc('In progress', 'Search index rebuild', {labels: ['Feature'], members: ['sam'], due: day(3),
  tasks: {items: [['Benchmark the current index', 1], ['Incremental rebuild', 1], ['Backfill old boards', 0], ['Document the rollout', 0]]}});
await pc('In progress', 'SSO group mapping', {labels: ['Feature'], members: ['jordan'], due: day(5)});
const launch = await pc('In progress', 'Apollo 2.0 launch checklist', {labels: ['Feature'], members: ['avery', 'jordan'], due: day(7),
  desc: 'Everything that has to be true before we ship **Apollo 2.0**.\n\n- Release notes reviewed by the whole team\n- Staging soak test runs for 48 hours\n- Support has the new runbook',
  tasks: {name: 'Before launch', items: [['Freeze the release branch', 1], ['Run the soak test', 1], ['Publish the release notes', 0], ['Brief the support team', 0]]},
  comments: [['jordan', 'Soak test is green after the cache fix. Looking good for Thursday.'],
             ['sam', 'Release notes draft is up. Could someone check the migration section?'],
             ['avery', 'On it, I will review the notes this afternoon.']]});
const triage = await pc('In progress', 'Triage new crash reports', {labels: ['Bug'], due: day(1),
  desc: 'Group the new crash reports by stack trace, link duplicates and comment a short summary per group.'});
await pc('Review', 'Export boards to CSV', {labels: ['Feature'], members: ['jordan'], due: day(2)});
await pc('Review', 'Mobile navigation spacing', {labels: ['Bug'], members: ['riley'], due: day(4)});
await pc('Done', 'Rotate staging certificates', {labels: ['Chore'], members: ['sam']});
await pc('Done', 'Upgrade the database driver', {labels: ['Chore'], members: ['jordan']});
await pc('Done', 'Dark-mode icon pass', {labels: ['Feature'], members: ['riley']});

// Apollo 2.0 timeline: a plain board, phases as columns, dated cards, milestones as a label
const tl = await A('POST', `/projects/${apollo.id}/boards`, {name: 'Apollo 2.0 timeline'});
const T = {lists: {}, labels: {}};
for (const n of ['Plan', 'Build', 'Beta', 'Launch', 'Done']) T.lists[n] = (await A('POST', `/boards/${tl.id}/lists`, {name: n, type: 'active'})).id;
await A('PATCH', `/lists/${T.lists['Done']}`, {color: 'green'});
for (const [n, c] of [['Milestone', 'berry-red'], ['Engineering', 'lagoon-blue'], ['Design', 'lilac-eyes'], ['Marketing', 'pumpkin-orange'], ['Docs', 'bright-moss']])
  T.labels[n] = (await A('POST', `/boards/${tl.id}/labels`, {name: n, color: c})).id;
await members(tl.id, ['jordan', 'sam', 'riley']);
const tc = maker(T);
await tc('Done', 'Kickoff and scope freeze', {labels: ['Milestone'], members: ['avery'], due: day(-9), done: true});
await tc('Done', 'Architecture review', {labels: ['Engineering'], members: ['jordan'], due: day(-3), done: true});
await tc('Build', 'API v2 endpoints', {labels: ['Engineering'], members: ['jordan'], due: dom(8)});
await tc('Build', 'New onboarding flow', {labels: ['Design'], members: ['riley'], due: dom(10)});
await tc('Build', 'Data migration tooling', {labels: ['Engineering'], members: ['sam'], due: dom(12)});
await tc('Beta', 'Beta opens to 50 teams', {labels: ['Milestone'], members: ['avery'], due: dom(15)});
await tc('Beta', 'Collect beta feedback', {labels: ['Design'], members: ['riley'], due: dom(18)});
await tc('Beta', 'Performance budget check', {labels: ['Engineering'], members: ['sam'], due: dom(19)});
await tc('Launch', 'Release candidate', {labels: ['Milestone'], members: ['jordan'], due: dom(22)});
await tc('Launch', 'Docs and release notes', {labels: ['Docs'], members: ['sam'], due: dom(23)});
await tc('Launch', 'Launch blog post', {labels: ['Marketing'], members: ['avery'], due: dom(25)});
await tc('Launch', 'Apollo 2.0 general availability', {labels: ['Milestone'], members: ['avery', 'jordan'], due: dom(27)});
await tc('Plan', 'Pricing page update', {labels: ['Marketing'], due: dom(24)});
await tc('Plan', 'Customer webinar', {labels: ['Marketing'], members: ['riley'], due: dom(29)});
for (const id of Object.values(T.lists)) await A('POST', `/lists/${id}/sort`, {field: 'dueDate'});

// two more boards straight from built-in templates, with their sample cards
const bugs = await A('POST', `/projects/${apollo.id}/boards/from-template`, {templateId: 'builtin:bug-tracker', name: 'Bug triage', sampleCards: true});
await members(bugs.id, ['sam', 'riley']);
const rel = await A('POST', `/projects/${apollo.id}/boards/from-template`, {templateId: 'builtin:release-checklist', name: 'Release 2.0 checklist', sampleCards: true});
await members(rel.id, ['jordan']);

// ---------------------------------------------------------------- an agent on the platform board
const agent = await A('POST', '/agents', {name: 'Triage agent', boardIds: [plat.id]});
const agentKey = agent.key && (agent.key.key || agent.key.apiKey || agent.key.token);
if (!agentKey) throw new Error('agent key missing: ' + JSON.stringify(Object.keys(agent.key || {})));
fs.writeFileSync(DIR + '/agent.key', agentKey, {mode: 0o600});
await api(agentKey, 'POST', `/cards/${triage.id}/claim`, {ttlSeconds: 86400, note: 'grouping reports by stack trace'});
await api(agentKey, 'POST', `/cards/${triage.id}/comments`, {text: 'Claimed. Found 14 reports, 3 distinct stack traces so far. Summary to follow.'});

// clean slate: no unread badges, and no human API keys (browser sessions only)
for (const u of ['avery', 'jordan', 'sam', 'riley']) await api(u, 'POST', '/notifications/read-all').catch(() => {});
for (const u of ['jordan', 'sam', 'riley', 'avery']) await api(u, 'DELETE', `/users/${sess[u].id}/api-key`);

fs.writeFileSync(DIR + '/state.json', JSON.stringify({
  personal: personal.id, week: week.id, apollo: apollo.id, platform: plat.id, timeline: tl.id, bugs: bugs.id, release: rel.id,
  platformLists: P.lists, timelineMonth, launch: launch.id, triage: triage.id, agent: agent.id,
}, null, 1));
console.log('seeded', DIR + '/state.json');
