# web

Web apps through playwright-cli. Assets: `context.md` (login steps, never:) and `pages/*.md` (path, elements, actions, checks). click/fill/press/select/do mutate only via tmvp write.

## Fields
base_url, users: {name: {role, user, password (secret)}}, browser, login: {manual: browser}

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `login` | read | login [-F user=ops] — run the scripted login (manual: browser → tmvp login <env> <system>) |
| `goto` | read | goto <page\|/path> — navigate (base_url + path) |
| `text` | read | text <el\|page.el> — visible text of an element |
| `snapshot` | read | snapshot [el] — accessibility snapshot (refs e1, e2 ...) |
| `screenshot` | read | screenshot [el] — PNG under ~/.tmvp/<product>/web/shots |
| `check` | read | check [page\|--all] — run the page checks |
| `ls` | read | ls — declared pages across all asset roots (path, elements, actions, source); offline |
| `pages` | read | pages — declared pages, paths, actions |
| `elements` | read | elements <page> — declared elements, locators, mutating/never |
| `click` | read-write | click <el> — read: non-mutating elements in destructive envs; else tmvp write ... click <el> --reason |
| `fill` | read-write | fill <el> <value> — as click (value never on argv) |
| `press` | read-write | press <key> — always mutating (may submit): tmvp write ... press <key> --reason |
| `select` | read-write | select <el> <value> — as click |
| `do` | read-write, default | do <action> [k=v ...] — page action; mutating ones via tmvp write (--preview resolves only) |
<!-- END verbs -->

## Discovery
`ls` = declared pages across asset roots (path, elements, actions, source) — works offline, no browser needed; `elements <page>` lists locators.

## Examples
```
tmvp prod admin ls
tmvp prod admin elements users
tmvp prod admin text users.rows
tmvp write prod admin do users.find-user email=a@b.test --reason "support #812"
```
