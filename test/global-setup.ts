import { DataSource } from 'typeorm';
import { validateEnv } from '../src/config/env.validation';
import { buildDataSourceOptions } from '../src/config/typeorm.config';
import { loadTestEnv } from './test-env';

// Schema comes from the real migrations, never synchronize, so the tests
// exercise exactly the schema production gets.
export default async function globalSetup(): Promise<void> {
  loadTestEnv();
  const dataSource = new DataSource(
    buildDataSourceOptions(validateEnv(process.env)),
  );
  await dataSource.initialize();
  try {
    await dataSource.runMigrations();
  } finally {
    await dataSource.destroy();
  }
}
