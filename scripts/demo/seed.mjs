// Seeds fictional demo data over the REST API (called by seed.sh). All names, boards and addresses are invented.
import fs from 'fs';

const BASE = process.env.WEAVE_BASE_URL;
const DIR = process.env.DEMO_DIR;
const keys = JSON.parse(fs.readFileSync(DIR + '/keys.json', 'utf8'));
const sess = JSON.parse(fs.readFileSync(DIR + '/sessions.json', 'utf8'));

async function api(who, method, path, body) {
  const r = await fetch(BASE + '/api/v1' + path, {
    method, headers: {Authorization: 'Bearer ' + keys[who], 'Content-Type': 'application/json'},
    body: body ? JSON.stringify(body) : undefined,
  });
  const j = await r.json();
  if (!r.ok) throw new Error(`${method} ${path}: ${JSON.stringify(j)}`);
  return j.item ?? j;
}
const A = (m, p, b) => api('avery', m, p, b);
const day = n => { const d = new Date(Date.now() + n * 864e5); return d.toISOString().slice(0, 10) + 'T17:00'; };

const prj = await A('POST', '/projects', {name: 'Apollo Platform', description: 'The platform team: API, search, mobile and release work.'});
const prj2 = await A('POST', '/projects', {name: 'Website Relaunch', description: 'New marketing site, content and launch plan.'});
const board = await A('POST', `/projects/${prj.id}/boards`, {name: 'Platform Sprint'});
const names = ['Backlog', 'In progress', 'Review', 'Done'];
const L = {};
for (const n of names) L[n] = (await A('POST', `/boards/${board.id}/lists`, {name: n, type: 'active'})).id;
await A('PATCH', `/lists/${L['In progress']}`, {wipLimit: 3, color: 'blue', description: 'Cards being built right now. Keep it to three at a time.'});
await A('PATCH', `/lists/${L['Done']}`, {color: 'green'});

for (const u of ['jordan', 'sam', 'riley']) await A('POST', `/boards/${board.id}/members`, {userId: sess[u].id, role: 'editor'});

const lab = {};
for (const [n, c] of [['Bug', 'berry-red'], ['Feature', 'lagoon-blue'], ['Design', 'lilac-eyes'], ['Docs', 'bright-moss'], ['Infra', 'pumpkin-orange']])
  lab[n] = (await A('POST', `/boards/${board.id}/labels`, {name: n, color: c})).id;

async function card(list, name, o = {}) {
  const c = await A('POST', `/lists/${L[list]}/cards`, {name, description: o.desc, dueDate: o.due != null ? day(o.due) : undefined});
  for (const l of o.labels || []) await A('POST', `/cards/${c.id}/labels`, {labelId: lab[l]});
  for (const m of o.members || []) await A('POST', `/cards/${c.id}/members`, {userId: sess[m].id});
  if (o.tasks) {
    const tl = await A('POST', `/cards/${c.id}/task-lists`, {name: o.tasks.name || 'Checklist', showOnFrontOfCard: true});
    for (const [t, done] of o.tasks.items) await A('POST', `/task-lists/${tl.id}/tasks`, {name: t, isCompleted: !!done});
  }
  for (const [who, text] of o.comments || []) await api(who, 'POST', `/cards/${c.id}/comments`, {text});
  if (o.loc) await A('PUT', `/cards/${c.id}/location`, {lat: o.loc[0], lng: o.loc[1], locationName: o.loc[2]});
  return c;
}

await card('Backlog', 'Audit API rate limits', {labels: ['Infra'], due: 9, members: ['sam']});
await card('Backlog', 'Dark-mode icon pass', {labels: ['Design'], members: ['riley'], due: 12});
await card('Backlog', 'Write onboarding guide', {loc: [52.52, 13.4, 'Berlin'], labels: ['Docs'], due: 15});
await card('Backlog', 'Webhook retry backoff', {labels: ['Feature'], members: ['jordan']});
await card('Backlog', 'Fix timezone in digest emails', {labels: ['Bug'], due: 5, members: ['sam']});

await card('In progress', 'Search index rebuild', {labels: ['Feature'], members: ['sam'], due: 3,
  tasks: {items: [['Benchmark current index', 1], ['Add incremental rebuild', 1], ['Backfill old boards', 0], ['Document the rollout', 0]]}});
await card('In progress', 'SSO group mapping', {labels: ['Feature', 'Infra'], members: ['jordan'], due: 6});
const launch = await card('In progress', 'Launch checklist: Apollo 2.0', {labels: ['Feature', 'Docs'], members: ['avery', 'jordan'], due: 7,
  desc: 'Everything that has to be true before we ship **Apollo 2.0**.\n\n- Release notes are reviewed by the whole team\n- Staging soak test runs for 48 hours\n- Support has the new runbook\n\nOwner: Avery. Questions in the comments.',
  tasks: {name: 'Before launch', items: [['Freeze the release branch', 1], ['Run the soak test', 1], ['Publish the release notes', 0], ['Brief the support team', 0], ['Announce to customers', 0]]},
  comments: [['jordan', 'Soak test is green after the cache fix. Looking good for Thursday.'],
             ['sam', 'Release notes draft is up. Could someone double-check the migration section?'],
             ['avery', 'On it. I will review the notes this afternoon.']]});

await card('Review', 'Export boards to CSV', {loc: [38.72, -9.14, 'Lisbon'], labels: ['Feature'], members: ['jordan'], due: 2});
await card('Review', 'Mobile navigation spacing', {labels: ['Design', 'Bug'], members: ['riley'], due: 4});

await card('Done', 'Rotate staging certificates', {loc: [40.71, -74.0, 'New York'], labels: ['Infra'], members: ['sam']});
await card('Done', 'Upgrade the database driver', {labels: ['Infra'], members: ['jordan']});
await card('Done', 'Plan the Apollo 2.0 roadmap', {labels: ['Docs'], members: ['avery']});


// second project: a small board so the home page has two projects
const b2 = await A('POST', `/projects/${prj2.id}/boards`, {name: 'Launch Plan'});
for (const [n, cs] of [['Ideas', ['Homepage story', 'Pricing page copy']], ['Writing', ['Customer quotes']], ['Live', ['Contact form']]]) {
  const l = await A('POST', `/boards/${b2.id}/lists`, {name: n, type: 'active'});
  for (const c of cs) await A('POST', `/lists/${l.id}/cards`, {name: c});
}
for (const u of ['jordan', 'riley']) await A('POST', `/boards/${b2.id}/members`, {userId: sess[u].id, role: 'editor'});

// clean slate for recording: no unread badge, and no API key anywhere (browser sessions only)
await A('POST', '/notifications/read-all');
for (const u of ['jordan', 'sam', 'riley']) await api(u, 'POST', '/notifications/read-all').catch(() => {});
for (const u of ['jordan', 'sam', 'riley', 'avery']) await api(u, 'DELETE', `/users/${sess[u].id}/api-key`);

fs.writeFileSync(DIR + '/state.json', JSON.stringify({project: prj.id, project2: prj2.id, board: board.id, board2: b2.id, lists: L, launch: launch.id, labels: lab}, null, 1));
console.log('seeded', DIR + '/state.json');
