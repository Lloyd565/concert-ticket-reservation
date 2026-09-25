import { durationToSeconds, validateEnv } from './env.validation';

const base = {
  DB_HOST: 'localhost',
  DB_PORT: '5433',
  DB_USERNAME: 'u',
  DB_PASSWORD: 'p',
  DB_NAME: 'db',
  JWT_SECRET: 'x'.repeat(32),
  ADMIN_EMAIL: 'admin@example.com',
  ADMIN_PASSWORD: 'admin12345',
};

describe('env validation', () => {
  it('converts durations to seconds', () => {
    expect(durationToSeconds('900s')).toBe(900);
    expect(durationToSeconds('15m')).toBe(900);
    expect(durationToSeconds('2h')).toBe(7200);
    expect(durationToSeconds('1d')).toBe(86400);
  });

  it('defaults JWT_EXPIRES_IN to 15m', () => {
    expect(validateEnv(base).JWT_EXPIRES_IN).toBe('15m');
  });

  it.each([
    ['a short JWT_SECRET', { JWT_SECRET: 'too-short' }],
    ['a malformed JWT_EXPIRES_IN', { JWT_EXPIRES_IN: '15 minutes' }],
    ['a non-numeric DB_PORT', { DB_PORT: 'abc' }],
  ])('rejects %s', (_, override) => {
    expect(() => validateEnv({ ...base, ...override })).toThrow(
      'Invalid environment configuration',
    );
  });
});
