import { Role } from './role';

export interface User {
  id: string;
  email: string;
  passwordHash: string;
  role: Role;
  createdAt: Date;
}

// Emails are compared and stored lowercase so 'A@x.com' and 'a@x.com' are
// one account; the users table also has a CHECK constraint enforcing this.
export function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}
