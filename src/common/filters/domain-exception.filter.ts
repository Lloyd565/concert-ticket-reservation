import {
  ArgumentsHost,
  Catch,
  ExceptionFilter,
  HttpStatus,
} from '@nestjs/common';
import { Response } from 'express';
import { DomainError, DomainErrorKind } from '../domain/domain-error';

const HTTP_BY_KIND: Record<
  DomainErrorKind,
  { status: HttpStatus; error: string }
> = {
  NOT_FOUND: { status: HttpStatus.NOT_FOUND, error: 'Not Found' },
  CONFLICT: { status: HttpStatus.CONFLICT, error: 'Conflict' },
  UNAUTHORIZED: { status: HttpStatus.UNAUTHORIZED, error: 'Unauthorized' },
};

@Catch(DomainError)
export class DomainExceptionFilter implements ExceptionFilter<DomainError> {
  catch(error: DomainError, host: ArgumentsHost): void {
    const { status, error: label } = HTTP_BY_KIND[error.kind];
    host
      .switchToHttp()
      .getResponse<Response>()
      .status(status)
      .json({ statusCode: status, message: error.message, error: label });
  }
}
