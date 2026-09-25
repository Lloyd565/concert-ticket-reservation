import { INestApplication } from '@nestjs/common';
import { Test } from '@nestjs/testing';
import request from 'supertest';
import { App } from 'supertest/types';
import { DataSource } from 'typeorm';
import { AppModule } from '../../src/app.module';
import { AdminBootstrapService } from '../../src/modules/users/application/admin-bootstrap.service';

export const ADMIN = {
  email: process.env.ADMIN_EMAIL ?? '',
  password: process.env.ADMIN_PASSWORD ?? '',
};

export interface TestApp {
  app: INestApplication<App>;
  http: () => ReturnType<typeof request>;
  db: DataSource;
  /** Empties every table and recreates the bootstrap admin. */
  reset: () => Promise<void>;
  register: (email: string, password?: string) => Promise<string>;
  login: (email: string, password?: string) => Promise<string>;
  close: () => Promise<void>;
}

const DEFAULT_PASSWORD = 'rahasia123';

export async function createTestApp(): Promise<TestApp> {
  const moduleRef = await Test.createTestingModule({
    imports: [AppModule],
  }).compile();
  // The real AppModule: global pipe, filter and guards come with it.
  const app = moduleRef.createNestApplication<INestApplication<App>>();
  await app.init();
  const db = app.get(DataSource);
  const http = () => request(app.getHttpServer());

  const login = async (email: string, password = DEFAULT_PASSWORD) => {
    const res = await http()
      .post('/auth/login')
      .send({ email, password })
      .expect(200);
    return (res.body as { accessToken: string }).accessToken;
  };

  return {
    app,
    http,
    db,
    reset: async () => {
      await db.query(
        'TRUNCATE reservations, concerts, users RESTART IDENTITY CASCADE',
      );
      await app.get(AdminBootstrapService).onApplicationBootstrap();
    },
    register: async (email, password = DEFAULT_PASSWORD) => {
      const res = await http()
        .post('/auth/register')
        .send({ email, password })
        .expect(201);
      return (res.body as { id: string }).id;
    },
    login,
    close: () => app.close(),
  };
}
