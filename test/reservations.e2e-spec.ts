import type { Test } from 'supertest';
import { ADMIN, createTestApp, TestApp } from './utils/test-app';

describe('Reservations (e2e)', () => {
  let t: TestApp;
  let admin: string;

  beforeAll(async () => {
    t = await createTestApp();
  });
  beforeEach(async () => {
    await t.reset();
    admin = await t.login(ADMIN.email, ADMIN.password);
  });
  afterAll(() => t.close());

  async function createConcert(capacity: number): Promise<string> {
    const res = await t
      .http()
      .post('/concerts')
      .set('Authorization', `Bearer ${admin}`)
      .send({
        name: 'Jazz Night',
        artist: 'Trio',
        venue: 'GBK',
        startsAt: '2026-12-31T19:00:00Z',
        capacity,
        price: 250000,
      })
      .expect(201);
    return (res.body as { id: string }).id;
  }

  const reserve = (token: string, concertId: string, quantity: number) =>
    t
      .http()
      .post('/reservations')
      .set('Authorization', `Bearer ${token}`)
      .send({ concertId, quantity });

  async function userToken(email: string): Promise<string> {
    await t.register(email);
    return t.login(email);
  }

  /** Straight from the database, not from what the API claims. */
  async function activeSeats(concertId: string): Promise<number> {
    const [row] = await t.db.query<{ seats: string }[]>(
      `SELECT COALESCE(SUM(quantity), 0) AS seats
         FROM reservations WHERE concert_id = $1 AND status = 'ACTIVE'`,
      [concertId],
    );
    return Number(row.seats);
  }

  it("user A cannot read, change or cancel user B's reservation (404)", async () => {
    const concertId = await createConcert(10);
    const a = await userToken('a@example.com');
    const b = await userToken('b@example.com');
    const res = await reserve(b, concertId, 2).expect(201);
    const id = (res.body as { id: string }).id;
    const asA = (req: Test) => req.set('Authorization', `Bearer ${a}`);

    await asA(t.http().get(`/reservations/${id}`)).expect(404);
    await asA(t.http().patch(`/reservations/${id}`))
      .send({ quantity: 1 })
      .expect(404);
    await asA(t.http().delete(`/reservations/${id}`)).expect(404);

    // Same answer as for an id that does not exist at all.
    await asA(
      t.http().get('/reservations/00000000-0000-4000-8000-000000000000'),
    ).expect(404);
    const listA = await asA(t.http().get('/reservations')).expect(200);
    expect(listA.body).toEqual([]);
    expect(await activeSeats(concertId)).toBe(2);
  });

  it('over-capacity request -> 409 and nothing is written', async () => {
    const concertId = await createConcert(5);
    const user = await userToken('a@example.com');

    await reserve(user, concertId, 4).expect(201);
    const res = await reserve(user, concertId, 2).expect(409);
    expect((res.body as { message: string }).message).toBe(
      'Not enough seats: 1 left',
    );
    expect(await activeSeats(concertId)).toBe(4);

    // Resizing counts too.
    const [{ id }] = (
      await t.http().get('/reservations').set('Authorization', `Bearer ${user}`)
    ).body as { id: string }[];
    await t
      .http()
      .patch(`/reservations/${id}`)
      .set('Authorization', `Bearer ${user}`)
      .send({ quantity: 6 })
      .expect(409);
    expect(await activeSeats(concertId)).toBe(4);
  });

  it('cancel frees the seats for someone else', async () => {
    const concertId = await createConcert(3);
    const a = await userToken('a@example.com');
    const b = await userToken('b@example.com');
    const res = await reserve(a, concertId, 3).expect(201);
    await reserve(b, concertId, 1).expect(409);

    const cancelled = await t
      .http()
      .delete(`/reservations/${(res.body as { id: string }).id}`)
      .set('Authorization', `Bearer ${a}`)
      .expect(200);
    expect((cancelled.body as { status: string }).status).toBe('CANCELLED');
    expect(await activeSeats(concertId)).toBe(0);

    await reserve(b, concertId, 3).expect(201);
    expect(await activeSeats(concertId)).toBe(3);
  });

  it('concurrency: N parallel reservations never exceed capacity K', async () => {
    // Kept well below what one wave of simultaneous requests asks for: with
    // capacity 7, the first wave (5 requests, 1+2+1+2+1 seats) filled it
    // exactly, so the test passed even with the lock removed.
    const CAPACITY = 3;
    const USERS = 5;
    const REQUESTS = 40;
    const concertId = await createConcert(CAPACITY);
    const tokens = await Promise.all(
      Array.from({ length: USERS }, (_, i) => userToken(`u${i}@example.com`)),
    );

    // Mixed sizes (1 and 2) so a naive check-then-insert could overshoot by
    // more than one seat, and several users so it is not one account's race.
    const results = await Promise.all(
      Array.from({ length: REQUESTS }, (_, i) =>
        reserve(tokens[i % USERS], concertId, (i % 2) + 1),
      ),
    );

    const statuses = results.map((r) => r.status);
    expect(statuses.every((s) => s === 201 || s === 409)).toBe(true);

    // The invariant, checked in the database rather than via HTTP codes.
    const seats = await activeSeats(concertId);
    expect(seats).toBeLessThanOrEqual(CAPACITY);
    // Demand (60 seats) far exceeds supply, so the lock must not have made us
    // turn away requests that fit: at most one seat can be left over, when
    // only 2-seat requests remained.
    expect(seats).toBeGreaterThanOrEqual(CAPACITY - 1);

    // Every 201 corresponds to exactly one stored ACTIVE row.
    const [{ rows }] = await t.db.query<{ rows: string }[]>(
      `SELECT COUNT(*) AS rows FROM reservations
        WHERE concert_id = $1 AND status = 'ACTIVE'`,
      [concertId],
    );
    expect(Number(rows)).toBe(statuses.filter((s) => s === 201).length);
  });
});
