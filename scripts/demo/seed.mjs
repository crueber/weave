// Seeds fictional demo data over the REST API (called by seed.sh). All names, boards and addresses are invented.
// Ownership follows the product: Avery (an admin) manages the Apollo Platform project, Jordan owns its sprint board and
// set up the board's agent and webhook, Riley created the Support Desk board (the project lets members create boards),
// and Jordan owns the Website Relaunch project. The launch checklist card sits on two boards. Morgan created a project
// and was then deactivated, so the admin console has an orphaned project; adding people to a group fills the audit log.
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
  const t = await r.text();
  const j = t ? JSON.parse(t) : {};
  if (!r.ok) throw new Error(`${method} ${path}: ${t}`);
  return j.item ?? j;
}
const A = (m, p, b) => api('avery', m, p, b);
const J = (m, p, b) => api('jordan', m, p, b);
const R = (m, p, b) => api('riley', m, p, b);
const day = n => { const d = new Date(Date.now() + n * 864e5); return d.toISOString().slice(0, 10) + 'T17:00'; };

const prj = await A('POST', '/projects', {name: 'Apollo Platform', description: 'The platform team: API, search, mobile and release work.'});
const board = await A('POST', `/projects/${prj.id}/boards`, {name: 'Platform Sprint'});
const names = ['Backlog', 'In progress', 'Review', 'Done'];
const L = {};
for (const n of names) L[n] = (await A('POST', `/boards/${board.id}/lists`, {name: n, type: 'active'})).id;
await A('PATCH', `/lists/${L['Backlog']}`, {description: 'Ideas and requests that are ready to be picked up. Each card says what done means.'});
await A('PATCH', `/lists/${L['In progress']}`, {wipLimit: 4, color: 'blue', description: 'Cards being built right now. Keep it to four at a time.'});
await A('PATCH', `/lists/${L['Review']}`, {description: 'Waiting for a second pair of eyes.'});
await A('PATCH', `/lists/${L['Done']}`, {color: 'green', type: 'closed', archiveAfter: 20, description: 'Shipped. Cards close when they land here; the column keeps the newest 20.'});
await A('PATCH', `/boards/${board.id}`, {hasInbox: true});

await A('POST', `/boards/${board.id}/members`, {userId: sess.jordan.id, role: 'owner'});
for (const u of ['sam', 'riley']) await A('POST', `/boards/${board.id}/members`, {userId: sess[u].id, role: 'editor'});
await A('PATCH', `/projects/${prj.id}`, {boardCreation: 'members'});

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
await card('In progress', 'Mobile push notifications', {labels: ['Feature'], members: ['riley'], due: 8});

const csv = await card('Review', 'Export boards to CSV', {loc: [38.72, -9.14, 'Lisbon'], labels: ['Feature'], members: ['jordan'], due: 2});
await card('Review', 'Mobile navigation spacing', {labels: ['Design', 'Bug'], members: ['riley'], due: 4});

await card('Done', 'Rotate staging certificates', {loc: [40.71, -74.0, 'New York'], labels: ['Infra'], members: ['sam']});
await card('Done', 'Upgrade the database driver', {labels: ['Infra'], members: ['jordan']});
await card('Done', 'Plan the Apollo 2.0 roadmap', {labels: ['Docs'], members: ['avery']});

// the board Inbox: ideas captured before they go on the board
for (const [who, n] of [['jordan', 'Offline mode for the mobile app'], ['sam', 'Status page for the API'], ['riley', 'Keyboard shortcuts cheat sheet']])
  await api(who, 'POST', `/boards/${board.id}/drafts`, {name: n});

// Avery watches the sprint board and the Review column (Settings > Watching; the bell lights up on new comments)
await A('PUT', `/boards/${board.id}/subscription`);
await A('PUT', `/lists/${L['Review']}/subscription`);

// an agent and a board webhook, created by the board's owner Jordan (board settings > Agents / Webhooks). The webhook
// listens only to events the shots and the demo never fire on this board, so nothing is ever POSTed to example.com.
const agent = await J('POST', '/agents', {name: 'Triage agent', boardIds: [board.id]});
await J('POST', '/webhooks', {name: 'Release bot', url: 'https://example.com/hooks/release', boardId: board.id, events: ['attachmentCreate', 'cardDelete', 'boardDelete']}).catch(e => console.log('webhook skipped:', e.message));

// second project, owned by Jordan: the marketing team's launch board. The launch checklist is on it too.
const prj2 = await J('POST', '/projects', {name: 'Website Relaunch', description: 'New marketing site, content and launch plan.'});
const b2 = await J('POST', `/projects/${prj2.id}/boards`, {name: 'Launch Plan'});
const L2 = {};
for (const [n, cs] of [['Ideas', ['Homepage story', 'Pricing page copy']], ['Writing', ['Customer quotes', 'Launch blog post']], ['Live', ['Contact form']]]) {
  const l = await J('POST', `/boards/${b2.id}/lists`, {name: n, type: n === 'Live' ? 'closed' : 'active'});
  L2[n] = l.id;
  for (const c of cs) await J('POST', `/lists/${l.id}/cards`, {name: c});
}
for (const u of ['avery', 'riley']) await J('POST', `/boards/${b2.id}/members`, {userId: sess[u].id, role: 'editor'});
await A('POST', `/cards/${launch.id}/placements`, {listId: L2['Writing']});

// Support Desk: a board Riley created in Avery's project (Riley owns it). The demo adds a card to it and applies a template.
const desk = await R('POST', `/projects/${prj.id}/boards`, {name: 'Support Desk'});
const L3 = {};
for (const [n, cs] of [['To do', ['Customer cannot export a board', 'Password reset email is slow']], ['Doing', ['Question about API limits']], ['Done', ['Typo on the pricing page']]]) {
  const l = await R('POST', `/boards/${desk.id}/lists`, {name: n, type: n === 'Done' ? 'closed' : 'active'});
  L3[n] = l.id;
  for (const c of cs) await R('POST', `/lists/${l.id}/cards`, {name: c});
}
for (const u of ['sam']) await R('POST', `/boards/${desk.id}/members`, {userId: sess[u].id, role: 'editor'});

// an orphaned project: Morgan created it, then left (deactivated), so no manager is active
const old = await api('morgan', 'POST', '/projects', {name: 'Office Move', description: 'Boxes, desks and the new floor plan.'});
await api('morgan', 'POST', `/projects/${old.id}/boards`, {name: 'Moving day'});
await api('morgan', 'DELETE', `/users/${sess.morgan.id}/api-key?all=true`);
await A('POST', `/users/${sess.morgan.id}/deactivate`);

// admin audit log: a group with a self-add and two adds (no key rotation: its log line shows a key prefix)
const grp = await A('POST', '/user-groups', {name: 'Platform team'});
for (const u of ['avery', 'jordan', 'sam']) await A('POST', `/user-groups/${grp.id}/members`, {userId: sess[u].id});

// clean slate for recording: no unread badge, and no API key anywhere (browser sessions only)
for (const u of ['avery', 'jordan', 'sam', 'riley']) await api(u, 'POST', '/notifications/read-all').catch(() => {});
for (const u of ['jordan', 'sam', 'riley', 'avery']) await api(u, 'DELETE', `/users/${sess[u].id}/api-key?all=true`);

fs.writeFileSync(DIR + '/state.json', JSON.stringify({project: prj.id, project2: prj2.id, board: board.id, board2: b2.id, desk: desk.id, lists: L, lists2: L2, lists3: L3,
  launch: launch.id, csv: csv.id, labels: lab, agent: agent.id}, null, 1));
console.log('seeded', DIR + '/state.json');
