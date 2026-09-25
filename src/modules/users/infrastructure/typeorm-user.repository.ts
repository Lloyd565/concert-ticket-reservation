import { Injectable } from '@nestjs/common';
import { InjectRepository } from '@nestjs/typeorm';
import { QueryFailedError, Repository } from 'typeorm';
import { Role } from '../domain/role';
import { User } from '../domain/user';
import { EmailAlreadyRegisteredError } from '../domain/user.errors';
import { NewUser, UserRepository } from '../domain/user.repository';
import { UserOrmEntity } from './user.entity';

const PG_UNIQUE_VIOLATION = '23505';

@Injectable()
export class TypeOrmUserRepository implements UserRepository {
  constructor(
    @InjectRepository(UserOrmEntity)
    private readonly users: Repository<UserOrmEntity>,
  ) {}

  async create(user: NewUser): Promise<User> {
    try {
      return toDomain(await this.users.save(this.users.create(user)));
    } catch (err) {
      // Rely on the unique index rather than a prior SELECT: two concurrent
      // registrations for one email would both pass a SELECT.
      if (
        err instanceof QueryFailedError &&
        (err.driverError as { code?: string }).code === PG_UNIQUE_VIOLATION
      ) {
        throw new EmailAlreadyRegisteredError();
      }
      throw err;
    }
  }

  async findById(id: string): Promise<User | null> {
    const row = await this.users.findOneBy({ id });
    return row && toDomain(row);
  }

  async findByEmail(email: string): Promise<User | null> {
    const row = await this.users.findOneBy({ email });
    return row && toDomain(row);
  }

  existsWithRole(role: Role): Promise<boolean> {
    return this.users.existsBy({ role });
  }
}

function toDomain(row: UserOrmEntity): User {
  return {
    id: row.id,
    email: row.email,
    passwordHash: row.passwordHash,
    role: row.role,
    createdAt: row.createdAt,
  };
}
