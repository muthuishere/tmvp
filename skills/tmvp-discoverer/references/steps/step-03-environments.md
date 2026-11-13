# Step 3 — environments (per project; later projects: "same as <first> except…?")

Show env names with evidence and markers; propose merges (`production`/`prod`, `application-dev.env`/`.env.dev`) and drop non-envs (`sample`, `example`, `ci`, `test`).

Round A: names · each env's `type:` (prod / staging / dev / test / local — there may be many envs of one type, e.g. 100 dev or 20 prod) · default env (questions.md step 3 A). Write `type:` on every env; prod-type envs are always protected.
Round B: shared stores between envs · `local` · several targets per env · naming vs sec scope (step 3 B).
A store shared with prod is protected in every env that uses it — say so when it happens.

Stop after each round. Record in `{session}`. Then step 4.
