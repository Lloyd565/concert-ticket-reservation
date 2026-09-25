// Implicit type conversion reads design:type metadata; load it here rather
// than relying on Nest having imported it first (unit tests, migration CLI).
import 'reflect-metadata';
import { plainToInstance } from 'class-transformer';
import {
  IsEmail,
  IsIn,
  IsInt,
  IsNotEmpty,
  IsString,
  Matches,
  Max,
  Min,
  MinLength,
  validateSync,
} from 'class-validator';

export class EnvironmentVariables {
  @IsIn(['development', 'test', 'production'])
  NODE_ENV: 'development' | 'test' | 'production' = 'development';

  @IsInt()
  @Min(1)
  @Max(65535)
  PORT: number = 3000;

  @IsString()
  @IsNotEmpty()
  DB_HOST: string;

  @IsInt()
  @Min(1)
  @Max(65535)
  DB_PORT: number;

  @IsString()
  @IsNotEmpty()
  DB_USERNAME: string;

  @IsString()
  @IsNotEmpty()
  DB_PASSWORD: string;

  @IsString()
  @IsNotEmpty()
  DB_NAME: string;

  // HS256 is only as strong as its key; 32 bytes matches the hash output size.
  @IsString()
  @MinLength(32)
  JWT_SECRET: string;

  @Matches(/^\d+[smhd]$/, {
    message: 'JWT_EXPIRES_IN must look like 900s, 15m, 1h or 1d',
  })
  JWT_EXPIRES_IN: string = '15m';

  @IsEmail()
  ADMIN_EMAIL: string;

  @IsString()
  @MinLength(8)
  ADMIN_PASSWORD: string;
}

const SECONDS_PER_UNIT = { s: 1, m: 60, h: 3600, d: 86400 } as const;

/** Converts a validated duration such as '15m' into seconds. */
export function durationToSeconds(duration: string): number {
  const unit = duration.slice(-1) as keyof typeof SECONDS_PER_UNIT;
  return Number(duration.slice(0, -1)) * SECONDS_PER_UNIT[unit];
}

// Runs at boot (ConfigModule) and in the migration CLI, so a missing or
// malformed variable stops the process before anything touches the database.
export function validateEnv(
  config: Record<string, unknown>,
): EnvironmentVariables {
  const env = plainToInstance(EnvironmentVariables, config, {
    enableImplicitConversion: true,
  });
  const errors = validateSync(env, { skipMissingProperties: false });
  if (errors.length > 0) {
    const details = errors
      .map(
        (e) =>
          `${e.property}: ${Object.values(e.constraints ?? {}).join(', ')}`,
      )
      .join('\n  ');
    throw new Error(`Invalid environment configuration:\n  ${details}`);
  }
  return env;
}
