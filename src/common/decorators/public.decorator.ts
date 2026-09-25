import { SetMetadata } from '@nestjs/common';

export const IS_PUBLIC_KEY = 'isPublic';

/** Opts a route out of the global JwtAuthGuard. Everything else needs a token. */
export const Public = () => SetMetadata(IS_PUBLIC_KEY, true);
