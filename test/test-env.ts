import { config } from 'dotenv';
import { resolve } from 'path';

/**
 * Loads .env.test over whatever the shell has, then refuses to continue unless
 * the target database is a *_test one: every test run TRUNCATEs all tables, so
 * pointing it at the dev database by mistake would wipe it.
 */
export function loadTestEnv(): void {
  config({
    path: resolve(__dirname, '..', '.env.test'),
    override: true,
    quiet: true,
  });
  process.env.ENV_FILE = '.env.test';
  const dbName = process.env.DB_NAME ?? '';
  if (!dbName.endsWith('_test')) {
    throw new Error(
      `Refusing to run e2e tests against "${dbName}": DB_NAME must end in _test`,
    );
  }
}
