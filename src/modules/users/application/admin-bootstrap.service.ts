import { Injectable, Logger, OnApplicationBootstrap } from '@nestjs/common';
import { ConfigService } from '@nestjs/config';
import { EnvironmentVariables } from '../../../config/env.validation';
import { Role } from '../domain/role';
import { EmailAlreadyRegisteredError } from '../domain/user.errors';
import { UserRepository } from '../domain/user.repository';
import { UsersService } from './users.service';

@Injectable()
export class AdminBootstrapService implements OnApplicationBootstrap {
  private readonly logger = new Logger(AdminBootstrapService.name);

  constructor(
    private readonly users: UserRepository,
    private readonly usersService: UsersService,
    private readonly config: ConfigService<EnvironmentVariables, true>,
  ) {}

  async onApplicationBootstrap(): Promise<void> {
    if (await this.users.existsWithRole(Role.ADMIN)) return;
    const email = this.config.get('ADMIN_EMAIL', { infer: true });
    try {
      await this.usersService.createUser(
        email,
        this.config.get('ADMIN_PASSWORD', { infer: true }),
        Role.ADMIN,
      );
      this.logger.log(`Created initial admin ${email}`);
    } catch (err) {
      if (!(err instanceof EmailAlreadyRegisteredError)) throw err;
      this.logger.warn(`Admin not created: ${email} is already registered`);
    }
  }
}
