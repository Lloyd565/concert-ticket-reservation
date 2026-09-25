import { config } from 'dotenv';
import { DataSource } from 'typeorm';
import { validateEnv } from '../config/env.validation';
import { buildDataSourceOptions } from '../config/typeorm.config';

// Entry point for the TypeORM CLI (migration:generate/run/revert). ENV_FILE
// lets the same scripts target the test database via .env.test.
config({ path: process.env.ENV_FILE ?? '.env', quiet: true });

export default new DataSource(buildDataSourceOptions(validateEnv(process.env)));
