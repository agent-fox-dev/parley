import { describe, expect, it } from 'vitest';
import { HttpError, errorResponse, renderErrorEnvelope } from './errors.js';

describe('renderErrorEnvelope', () => {
  const cases: Array<{ name: string; status: number; message: string; errorType?: string }> = [
    { name: 'a bad request with no class', status: 400, message: 'the body is not a JSON object' },
    { name: 'a not found with no class', status: 404, message: 'workspace not found' },
    {
      name: 'a conflict with a class',
      status: 409,
      message: 'another operation is running',
      errorType: 'workspace_busy',
    },
    {
      name: 'a forbidden with a class',
      status: 403,
      message: 'org members have read-only access',
      errorType: 'forbidden',
    },
    { name: 'a server error with no class', status: 500, message: 'internal server error' },
  ];

  it.each(cases)('carries the status and message for $name', ({ status, message, errorType }) => {
    const body = renderErrorEnvelope(new HttpError(status, message, errorType));

    expect(body.error.code).toBe(status);
    expect(body.error.message).toBe(message);
    expect(body.error.message.length).toBeGreaterThan(0);
  });

  it.each(cases)(
    'includes error_type only when one was raised for $name',
    ({ status, message, errorType }) => {
      const body = renderErrorEnvelope(new HttpError(status, message, errorType));

      expect(Object.hasOwn(body.error, 'error_type')).toBe(errorType !== undefined);
      if (errorType !== undefined) {
        expect(body.error.error_type).toBe(errorType);
      }
    },
  );

  it('omits error_type entirely rather than setting it to null', () => {
    const body = renderErrorEnvelope(new HttpError(404, 'not found'));

    expect(JSON.stringify(body)).not.toContain('error_type');
    expect(Object.keys(body.error)).toEqual(['code', 'message']);
  });
});

describe('errorResponse', () => {
  it('answers as JSON with the status the error carries', async () => {
    const response = errorResponse(
      new HttpError(409, 'a rebuild is already running', 'concurrent_rebuild'),
    );

    expect(response.status).toBe(409);
    expect(response.headers.get('content-type')).toContain('application/json');
    await expect(response.json()).resolves.toEqual({
      error: {
        code: 409,
        message: 'a rebuild is already running',
        error_type: 'concurrent_rebuild',
      },
    });
  });
});
