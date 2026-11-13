// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';

// The released version shown in the header. Never hardcoded: the nearest git tag,
// else the newest v* tag in the repo, else the top "## x.y.z" of ../CHANGELOG.md.
const changelog = new URL('../CHANGELOG.md', import.meta.url);
const hasChangelog = existsSync(changelog);
function releasedVersion() {
  const git = (...args) => {
    try { return execFileSync('git', args, { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim().split('\n')[0]; } catch { return ''; }
  };
  const tag = git('describe', '--tags', '--abbrev=0') || git('tag', '--list', 'v*', '--sort=-v:refname');
  if (tag) return tag.startsWith('v') ? tag : 'v' + tag;
  const m = hasChangelog && readFileSync(changelog, 'utf8').match(/^##\s+v?(\d+\.\d+\.\d+[^\s]*)/m);
  return m ? 'v' + m[1] : '';
}

// Deploy target is not decided yet; set SITE/BASE when it is (e.g. GitHub Pages:
// SITE=https://muthuishere.github.io BASE=/tmvp).
export default defineConfig({
  site: process.env.SITE || 'https://tmvp.dev',
  base: process.env.BASE || '/',
  // demos and commands must stay byte-exact: no curly quotes, no en-dashes for --flags
  markdown: { smartypants: false },
  vite: { define: { 'import.meta.env.TMVP_VERSION': JSON.stringify(releasedVersion()), 'import.meta.env.TMVP_HAS_CHANGELOG': JSON.stringify(hasChangelog) } },
  integrations: [
    starlight({
      title: 'T-mVp',
      description: 'One CLI for coding agents and engineers to read every system of a product, across every environment. Read-only by default, writes on purpose, secrets never seen.',
      logo: { light: './src/assets/logo-light.svg', dark: './src/assets/logo-dark.svg', alt: 'T-mVp' },
      favicon: '/favicon.svg',
      components: { Hero: './src/components/Hero.astro', SiteTitle: './src/components/SiteTitle.astro' },
      // restore the visitor's sidebar choice before first paint (see SiteTitle.astro)
      head: [{ tag: 'script', content: "try{if(localStorage.getItem('tm-sidebar')==='closed')document.documentElement.dataset.tmSidebar='closed'}catch{}" }],
      disable404Route: true,
      customCss: [
        '@fontsource-variable/geist',
        '@fontsource-variable/geist-mono',
        './src/styles/theme.css',
      ],
      social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/muthuishere/tmvp' }],
      expressiveCode: {
        themes: ['github-dark-default', 'github-light'],
        styleOverrides: {
          borderRadius: '0',
          codeFontFamily: 'var(--sl-font-mono)',
          codeFontSize: '0.85rem',
          frames: { frameBoxShadowCssValue: 'none' },
        },
      },
      sidebar: [
        { label: 'Start', items: [
          { label: 'Why T-mVp', slug: 'start/why' },
          { label: 'Install', slug: 'start/install' },
          { label: 'Set up your agent', slug: 'start/quickstart' },
          { label: 'Examples', slug: 'start/examples' },
        ] },
        { label: 'Concepts', items: [
          { label: 'Overview', slug: 'concepts/overview' },
          { label: 'Environments', slug: 'concepts/environments' },
          { label: 'Systems & types', slug: 'concepts/systems-and-types' },
          { label: 'Via chains', slug: 'concepts/via-chains' },
          { label: 'Reads, writes, destructive', slug: 'concepts/reads-and-writes' },
          { label: 'Limits, cost, timeouts', slug: 'concepts/limits-cost-timeouts' },
          { label: 'The local server', slug: 'concepts/local-server' },
          { label: 'Audit log', slug: 'concepts/audit-log' },
          { label: 'Logs & metrics', slug: 'concepts/logs-and-metrics' },
          { label: 'Skills', slug: 'concepts/skills' },
        ] },
        { label: 'Secrets', items: [
          { label: 'How secrets work', slug: 'secrets/how-it-works' },
          { label: '.env files & environment variables', slug: 'secrets/env-files' },
          { label: 'Secret managers', slug: 'secrets/managers' },
        ] },
        { label: 'Agent skills', items: [
          { label: 'tmvp-discoverer', slug: 'agent-skills/discoverer' },
          { label: 'Your <product>-tmvp skill', slug: 'agent-skills/product-skill' },
          { label: 'Discoverer steps', slug: 'agent-skills/discoverer-steps' },
          { label: 'Discoverer question bank', slug: 'agent-skills/discoverer-questions' },
        ] },
        { label: 'Systems', items: [{ autogenerate: { directory: 'systems' } }] },
        { label: 'Reference', items: [
          { label: 'Commands', slug: 'reference/commands' },
          { label: 'config.yaml', slug: 'reference/config' },
          { label: 'Errors & exit codes', slug: 'reference/errors' },
          { label: 'Troubleshooting', slug: 'reference/troubleshooting' },
          { label: 'Doctor', slug: 'reference/doctor' },
          { label: 'Install details', slug: 'reference/install-details' },
          ...(hasChangelog ? [{ label: 'Changelog', slug: 'reference/changelog' }] : []),
        ] },
        { label: 'Security model', items: [{ label: 'Security model', slug: 'security/model' }] },
      ],
    }),
  ],
});
