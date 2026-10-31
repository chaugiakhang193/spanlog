import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['src/**/*.spec.ts'],
    // Tests that need the Postgres container run with npm run test:pg.
    exclude: ['src/**/*.pg.spec.ts'],
  },
});
