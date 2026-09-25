import { User } from '../../users/domain/user';

/** What `request.user` holds after JwtStrategy has verified a token. */
export type AuthenticatedUser = Pick<User, 'id' | 'email' | 'role'>;
