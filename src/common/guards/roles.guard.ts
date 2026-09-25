import { CanActivate, ExecutionContext, Injectable } from '@nestjs/common';
import { Reflector } from '@nestjs/core';
import { Request } from 'express';
import { AuthenticatedUser } from '../../modules/auth/domain/authenticated-user';
import { Role } from '../../modules/users/domain/role';
import { ROLES_KEY } from '../decorators/roles.decorator';

// Runs after JwtAuthGuard, so request.user is already set on protected routes.
// Returning false makes Nest answer 403.
@Injectable()
export class RolesGuard implements CanActivate {
  constructor(private readonly reflector: Reflector) {}

  canActivate(ctx: ExecutionContext): boolean {
    const required = this.reflector.getAllAndOverride<Role[] | undefined>(
      ROLES_KEY,
      [ctx.getHandler(), ctx.getClass()],
    );
    if (!required?.length) return true;
    const user = ctx.switchToHttp().getRequest<Request>().user as
      AuthenticatedUser | undefined;
    return !!user && required.includes(user.role);
  }
}
