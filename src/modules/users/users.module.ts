import { Module } from '@nestjs/common';
import { TypeOrmModule } from '@nestjs/typeorm';
import { AdminBootstrapService } from './application/admin-bootstrap.service';
import { UsersService } from './application/users.service';
import { UserRepository } from './domain/user.repository';
import { TypeOrmUserRepository } from './infrastructure/typeorm-user.repository';
import { UserOrmEntity } from './infrastructure/user.entity';

@Module({
  imports: [TypeOrmModule.forFeature([UserOrmEntity])],
  providers: [
    UsersService,
    AdminBootstrapService,
    { provide: UserRepository, useClass: TypeOrmUserRepository },
  ],
  exports: [UsersService, UserRepository],
})
export class UsersModule {}
