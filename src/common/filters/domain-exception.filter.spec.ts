import { ArgumentsHost } from '@nestjs/common';
import { DomainError, DomainErrorKind } from '../domain/domain-error';
import { DomainExceptionFilter } from './domain-exception.filter';

class TestError extends DomainError {
  constructor(readonly kind: DomainErrorKind) {
    super('boom');
  }
}

function capture(kind: DomainErrorKind) {
  const res = { status: jest.fn(), json: jest.fn() };
  res.status.mockReturnValue(res);
  const host = {
    switchToHttp: () => ({ getResponse: () => res }),
  } as unknown as ArgumentsHost;
  new DomainExceptionFilter().catch(new TestError(kind), host);
  return res;
}

describe('DomainExceptionFilter', () => {
  it.each([
    ['NOT_FOUND', 404, 'Not Found'],
    ['CONFLICT', 409, 'Conflict'],
    ['UNAUTHORIZED', 401, 'Unauthorized'],
  ] as const)('maps %s to %i', (kind, status, error) => {
    const res = capture(kind);
    expect(res.status).toHaveBeenCalledWith(status);
    expect(res.json).toHaveBeenCalledWith({
      statusCode: status,
      message: 'boom',
      error,
    });
  });
});
