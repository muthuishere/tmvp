import { defineCollection } from 'astro:content';
import { docsLoader, i18nLoader } from '@astrojs/starlight/loaders';
import { docsSchema, i18nSchema } from '@astrojs/starlight/schema';

export const collections = {
	docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
	// UI strings (Starlight reads this collection; one English override keeps it non-empty).
	i18n: defineCollection({ loader: i18nLoader(), schema: i18nSchema() }),
};
