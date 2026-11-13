// After `astro build`: when the site is served under a sub-path (BASE=/tmvp,
// GitHub Pages), prefix root-relative href/src in the generated HTML that
// content wrote as "/secrets/…". Links Astro/Starlight already based are left.
import { readdirSync, readFileSync, writeFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

const base = (process.env.BASE || '/').replace(/\/?$/, '/');
if (base === '/') process.exit(0);
const walk = (d) => readdirSync(d).flatMap((f) => {
  const p = join(d, f);
  return statSync(p).isDirectory() ? walk(p) : p.endsWith('.html') ? [p] : [];
});
let n = 0;
for (const f of walk('dist')) {
  const s = readFileSync(f, 'utf8');
  const t = s.replace(/\b(href|src)="\/(?!\/)([^"]*)"/g, (m, a, rest) =>
    ('/' + rest).startsWith(base) ? m : (n++, `${a}="${base}${rest}"`));
  if (t !== s) writeFileSync(f, t);
}
console.log(`rebase-links: ${n} links prefixed with ${base}`);
