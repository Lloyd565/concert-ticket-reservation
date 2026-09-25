import { join } from 'path';
import { DataSourceOptions } from 'typeorm';
import { EnvironmentVariables } from './env.validation';

// Shared by the Nest app and the migration CLI so both see the same entities
// and migrations. Schema only ever changes through migrations.
export function buildDataSourceOptions(
  env: Pick<
    EnvironmentVariables,
    'DB_HOST' | 'DB_PORT' | 'DB_USERNAME' | 'DB_PASSWORD' | 'DB_NAME'
  >,
): DataSourceOptions {
  return {
    type: 'postgres',
    host: env.DB_HOST,
    port: env.DB_PORT,
    username: env.DB_USERNAME,
    password: env.DB_PASSWORD,
    database: env.DB_NAME,
    entities: [join(__dirname, '..', '**', '*.entity.{ts,js}')],
    migrations: [join(__dirname, '..', 'database', 'migrations', '*.{ts,js}')],
    synchronize: false,
    // gen_random_uuid() is built into Postgres 13+; the default needs uuid-ossp.
    uuidExtension: 'pgcrypto',
  };
}
