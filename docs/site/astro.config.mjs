/*
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// @ts-check
import { defineConfig } from 'astro/config';
import { unified } from '@astrojs/markdown-remark';
import starlight from '@astrojs/starlight';
import { remarkPrependBase } from './src/plugins/remark-prepend-base.mjs';

const BASE = '/in-cluster-observability';

// go-steer docs convention (mirrors go-steer/core-agent): Astro
// Starlight, base-independent content links via remarkPrependBase,
// light-only theme owned by src/styles/theme.css.
//
// baseURL matches the production GH Pages path so relative links
// resolve identically in dev and in prod.
export default defineConfig({
  site: 'https://gke-labs.github.io',
  base: BASE,
  markdown: {
    processor: unified({ remarkPlugins: [remarkPrependBase(BASE)] }),
  },
  integrations: [
    starlight({
      title: 'Ollie',
      description:
        'Transparent eBPF network observability for Kubernetes workloads — zero instrumentation, full K8s identity.',
      logo: undefined,
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/gke-labs/in-cluster-observability',
        },
      ],
      editLink: {
        baseUrl:
          'https://github.com/gke-labs/in-cluster-observability/edit/main/docs/site/',
      },
      // Inline script runs before Starlight's own ThemeProvider script,
      // pinning data-theme to 'light' before first paint. Belt-and-braces
      // with the theme.css overrides that already apply under both
      // [data-theme='light'] and [data-theme='dark'].
      head: [
        {
          tag: 'script',
          attrs: { 'is:inline': true },
          content: "document.documentElement.dataset.theme = 'light';",
        },
      ],
      // Palette + typography live in one file so the whole visual
      // system is swappable.
      customCss: ['./src/styles/theme.css'],
      // Empty component overrides drop the dark-mode toggle from the
      // navbar. Light-only site, same as core-agent.
      components: {
        ThemeSelect: './src/components/ThemeSelect.astro',
        ThemeProvider: './src/components/ThemeProvider.astro',
        Hero: './src/components/Hero.astro',
      },
      // Audience-first IA, sized to the current page set. Explicit
      // links (not autogenerate) while the set is small enough to
      // curate ordering by hand.
      sidebar: [
        {
          label: 'Overview',
          items: [
            { label: 'Introduction', link: '/' },
            { label: 'What works today', link: '/what-works-today/' },
          ],
        },
        {
          label: 'Use it',
          items: [
            { label: 'Getting started', link: '/getting-started/' },
            { label: 'Autoscale on captured traffic', link: '/use-cases/hpa-autoscaling/' },
          ],
        },
        {
          label: 'Understand it',
          items: [
            { label: 'Architecture', link: '/architecture/' },
            { label: 'Roadmap', link: '/roadmap/' },
          ],
        },
        {
          label: 'Contribute',
          items: [{ label: 'Contributing', link: '/contributing/' }],
        },
      ],
    }),
  ],
});
