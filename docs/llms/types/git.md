# git

Git repos, read only (never pushes). Bare clone cached per env/system, fetched at most once a minute.

## Fields
url, branch, key (`${file:secrets.x}`) or token (+ user)

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — branches and tags (kind, name, commit, date), newest first, --limit |
| `log` | read | log [rev] [-- path] — commits (hash, author, date, subject), --since / --limit |
| `diff` | read | diff [a] [b] [-- path] — default HEAD~1 HEAD; bounded text |
| `show` | read | show <rev>[:path] — commit or file at a revision; bounded text |
| `tags` | read | tags [pattern] — newest first |
| `deploys` | read | deploys [tag-pattern] — tags and commits since --since (default 7d) |
| `fetch` | read | fetch — refresh the local cache now |
<!-- END verbs -->

## Discovery
`ls` = branches and tags, newest first.

## Examples
```
tmvp all infra ls --limit 20
tmvp prod infra log --since 2d
tmvp prod infra deploys
tmvp prod infra show v1.4.2:helm/values.yaml
```
