# Step 4b — what will agents ask? (brainstorm, ~5 min, optional)

Goal: the saved-query (and saved-script/api) list, from real needs, not just code.

1. Ask: "Which 3 questions did you last answer by hand on prod?" Stop.
2. Offer 8–10 candidates seeded from code: repository/DAO queries, admin pages, job/status columns, error tables, the lookups code does most (by id, by email). Each: name · one-line description · params · system · envs.
3. One question per turn: pick, add, rename, drop. Stop when the user says "enough" (or `defaults` = top 5).
4. Apply sensitivity: no PII columns — ids and counts; every query cheap (indexed filter or `limit`).

Write the list to `{session}` → `## Saved queries`. Then step 5.
