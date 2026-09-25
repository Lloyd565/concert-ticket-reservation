import { config } from 'dotenv';
import { DataSource } from 'typeorm';
import { validateEnv } from '../config/env.validation';
import { buildDataSourceOptions } from '../config/typeorm.config';

config({ path: process.env.ENV_FILE ?? '.env', quiet: true });

export default new DataSource(buildDataSourceOptions(validateEnv(process.env)));
