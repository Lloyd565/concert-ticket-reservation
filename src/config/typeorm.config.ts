import { join } from 'path';
import { DataSourceOptions } from 'typeorm';
import { EnvironmentVariables } from './env.validation';

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
    uuidExtension: 'pgcrypto',
  };
}
