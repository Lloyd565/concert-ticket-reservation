import { JwtService } from '@nestjs/jwt';
import { ADMIN, createTestApp, TestApp } from './utils/test-app';

const b64url = (value: object) =>
  Buffer.from(JSON.stringify(value)).toString('base64url');

function decodePart<T>(token: string, index: 0 | 1): T {
  return JSON.parse(
    Buffer.from(token.split('.')[index], 'base64url').toString(),
  ) as T;
}

const concertBody = {
  name: 'Jazz Night',
  artist: 'Trio',
  venue: 'GBK',
  startsAt: '2026-12-31T19:00:00Z',
  capacity: 100,
  price: 250000,
};

describe('Auth & JWT (e2e)', () => {
  let t: TestApp;

  beforeAll(async () => {
    t = await createTestApp();
  });
  beforeEach(() => t.reset());
  afterAll(() => t.close());

  const me = (authorization?: string) => {
    const req = t.http().get('/auth/me');
    return authorization ? req.set('Authorization', authorization) : req;
  };

  // Signs with the real secret but no default expiry, so a test can set `exp`
  // itself; the app's JwtService would refuse an explicit exp.
  const realSigner = () => new JwtService({ secret: process.env.JWT_SECRET });

  it('1. register -> 201 without passwordHash', async () => {
    const res = await t
      .http()
      .post('/auth/register')
      .send({ email: 'Budi@Example.com', password: 'rahasia123' })
      .expect(201);

    expect(res.body).toEqual({
      id: expect.any(String) as unknown,
      email: 'budi@example.com',
      role: 'USER',
      createdAt: expect.any(String) as unknown,
    });
    expect(JSON.stringify(res.body)).not.toMatch(/password/i);
  });

  it('1b. register with a client-supplied role -> 400 (role is never settable)', async () => {
    await t
      .http()
      .post('/auth/register')
      .send({ email: 'x@example.com', password: 'rahasia123', role: 'ADMIN' })
      .expect(400);
  });

  it('2. register with a duplicate email -> 409 (case-insensitive)', async () => {
    await t.register('budi@example.com');
    await t
      .http()
      .post('/auth/register')
      .send({ email: 'BUDI@example.com', password: 'rahasia123' })
      .expect(409);
  });

  it('3. login -> 200 with an HS256 token carrying sub, role, exp', async () => {
    const id = await t.register('budi@example.com');
    const res = await t
      .http()
      .post('/auth/login')
      .send({ email: 'budi@example.com', password: 'rahasia123' })
      .expect(200);

    const { accessToken, expiresIn } = res.body as {
      accessToken: string;
      expiresIn: number;
    };
    expect(expiresIn).toBe(900);
    expect(decodePart<{ alg: string }>(accessToken, 0).alg).toBe('HS256');
    const payload = decodePart<Record<string, unknown>>(accessToken, 1);
    expect(payload).toMatchObject({ sub: id, role: 'USER' });
    expect(payload.exp).toEqual(expect.any(Number));
    expect((payload.exp as number) - (payload.iat as number)).toBe(900);
  });

  it('4. wrong password and unknown email -> identical 401', async () => {
    await t.register('budi@example.com');
    const wrongPassword = await t
      .http()
      .post('/auth/login')
      .send({ email: 'budi@example.com', password: 'salah12345' })
      .expect(401);
    const unknownEmail = await t
      .http()
      .post('/auth/login')
      .send({ email: 'nobody@example.com', password: 'salah12345' })
      .expect(401);

    expect(wrongPassword.body).toEqual(unknownEmail.body);
  });

  it('5. protected route without Authorization -> 401', async () => {
    await me().expect(401);
  });

  it.each([
    ['scheme only', 'Bearer'],
    ['non-Bearer scheme', 'Basic YnVkaTpyYWhhc2lhMTIz'],
    ['token without scheme', 'eyJhbGciOiJIUzI1NiJ9.e30.x'],
    ['garbage token', 'Bearer not-a-jwt'],
  ])('6. malformed header (%s) -> 401', async (_, header) => {
    await me(header).expect(401);
  });

  it('6b. a valid token under a non-Bearer scheme -> 401', async () => {
    await t.register('budi@example.com');
    const token = await t.login('budi@example.com');
    await me(`Token ${token}`).expect(401);
  });

  it('7. tampered token (payload altered or signature swapped) -> 401', async () => {
    await t.register('budi@example.com');
    await t.register('ani@example.com');
    const budi = await t.login('budi@example.com');
    const ani = await t.login('ani@example.com');
    const [header, payload, signature] = budi.split('.');

    // Privilege escalation attempt: same signature, payload says ADMIN.
    const escalated = b64url({ ...decodePart(budi, 1), role: 'ADMIN' });
    await me(`Bearer ${header}.${escalated}.${signature}`).expect(401);

    // Budi's header+payload with Ani's (valid, but different) signature.
    const aniSignature = ani.split('.')[2];
    await me(`Bearer ${header}.${payload}.${aniSignature}`).expect(401);
  });

  it('8. token signed with a different secret -> 401', async () => {
    const id = await t.register('budi@example.com');
    const forged = await new JwtService({
      secret: 'some-other-secret-that-is-also-32-chars-long',
    }).signAsync({ sub: id, role: 'USER' }, { expiresIn: 900 });
    await me(`Bearer ${forged}`).expect(401);
  });

  it('9. expired token signed with the real secret -> 401', async () => {
    const id = await t.register('budi@example.com');
    const now = Math.floor(Date.now() / 1000);
    const expired = await realSigner().signAsync({
      sub: id,
      role: 'USER',
      iat: now - 3600,
      exp: now - 60,
    });
    await me(`Bearer ${expired}`).expect(401);

    // Control: the same signer with a future exp is accepted, so the 401
    // above is caused by expiry and nothing else.
    const fresh = await realSigner().signAsync({
      sub: id,
      role: 'USER',
      exp: now + 60,
    });
    await me(`Bearer ${fresh}`).expect(200);
  });

  it('10. alg:none token -> 401', async () => {
    const id = await t.register('budi@example.com');
    const now = Math.floor(Date.now() / 1000);
    const unsigned = `${b64url({ alg: 'none', typ: 'JWT' })}.${b64url({
      sub: id,
      role: 'ADMIN',
      iat: now,
      exp: now + 900,
    })}.`;
    await me(`Bearer ${unsigned}`).expect(401);
  });

  it('11. valid token -> /auth/me returns that user', async () => {
    const id = await t.register('budi@example.com');
    const token = await t.login('budi@example.com');
    const res = await me(`Bearer ${token}`).expect(200);
    expect(res.body).toEqual({
      id,
      email: 'budi@example.com',
      role: 'USER',
    });
  });

  it('12. valid token of a deleted user -> 401', async () => {
    const id = await t.register('budi@example.com');
    const token = await t.login('budi@example.com');
    await me(`Bearer ${token}`).expect(200);

    await t.db.query('DELETE FROM users WHERE id = $1', [id]);
    await me(`Bearer ${token}`).expect(401);
  });

  it('13. RBAC: USER POST /concerts -> 403, ADMIN -> 201', async () => {
    await t.register('budi@example.com');
    const user = await t.login('budi@example.com');
    const admin = await t.login(ADMIN.email, ADMIN.password);

    await t
      .http()
      .post('/concerts')
      .set('Authorization', `Bearer ${user}`)
      .send(concertBody)
      .expect(403);
    await t
      .http()
      .post('/concerts')
      .set('Authorization', `Bearer ${admin}`)
      .send(concertBody)
      .expect(201);
  });

  it('14. public route GET /concerts works without a token', async () => {
    const res = await t.http().get('/concerts').expect(200);
    expect(res.body).toEqual({ items: [], page: 1, limit: 10, total: 0 });
  });
});
