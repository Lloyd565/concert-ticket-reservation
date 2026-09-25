// @ts-check
import eslint from '@eslint/js';
import eslintPluginPrettierRecommended from 'eslint-plugin-prettier/recommended';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  {
    ignores: ['eslint.config.mjs'],
  },
  eslint.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  eslintPluginPrettierRecommended,
  {
    languageOptions: {
      globals: {
        ...globals.node,
        ...globals.jest,
      },
      sourceType: 'commonjs',
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
  },
  {
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/no-floating-promises': 'warn',
      '@typescript-eslint/no-unsafe-argument': 'warn',
      'prettier/prettier': ['error', { endOfLine: 'auto' }],
    },
  },
  // Layer boundaries: dependencies point inward only
  // (presentation -> application -> domain <- infrastructure).
  layer('src/modules/*/domain/**/*.ts', {
    packages: ['@nestjs/*', 'typeorm', 'class-validator', 'class-transformer'],
    layers: ['presentation', 'application', 'infrastructure'],
    why: 'domain/ is plain TypeScript: no framework, no ORM, no outer layer.',
  }),
  layer('src/modules/*/application/**/*.ts', {
    packages: ['typeorm', '@nestjs/typeorm', 'pg'],
    layers: ['presentation', 'infrastructure'],
    why: 'services depend on domain ports, never on TypeORM or controllers.',
  }),
  layer('src/modules/*/presentation/**/*.ts', {
    packages: ['typeorm', '@nestjs/typeorm', 'pg'],
    layers: ['infrastructure'],
    why: 'controllers go through application services, never the database.',
  }),
  layer('src/modules/*/infrastructure/**/*.ts', {
    packages: [],
    layers: ['presentation', 'application'],
    why: 'infrastructure implements domain ports and knows nothing above them.',
  }),
);

/**
 * @param {string} files
 * @param {{ packages: string[], layers: string[], why: string }} opts
 */
function layer(files, { packages, layers, why }) {
  const patterns = [];
  if (packages.length) patterns.push({ group: packages, message: why });
  patterns.push({
    // Matches relative imports such as '../infrastructure/user.entity' or
    // '../../users/presentation/x', whichever module they point into.
    regex: `(^|/)(${layers.join('|')})(/|$)`,
    message: why,
  });
  return {
    files: [files],
    rules: { 'no-restricted-imports': ['error', { patterns }] },
  };
}
