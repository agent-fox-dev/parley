/**
 * The envelope every error of the service is answered in. `error_type` is a
 * machine-readable class, present only for the conditions the API reference
 * lists one for, and absent — not null — otherwise.
 */
export interface ErrorEnvelope {
  error: { code: number; message: string; error_type?: string };
}

/** An error carrying the status and, where there is one, the class to answer with. */
export class HttpError extends Error {
  readonly status: number;
  readonly errorType: string | undefined;

  constructor(status: number, message: string, errorType?: string) {
    super(message);
    this.name = 'HttpError';
    this.status = status;
    this.errorType = errorType;
  }
}

export function renderErrorEnvelope(err: HttpError): ErrorEnvelope {
  const error: ErrorEnvelope['error'] = { code: err.status, message: err.message };
  if (err.errorType !== undefined) {
    error.error_type = err.errorType;
  }
  return { error };
}

export function errorResponse(err: HttpError): Response {
  return new Response(JSON.stringify(renderErrorEnvelope(err)), {
    status: err.status,
    headers: { 'content-type': 'application/json; charset=UTF-8' },
  });
}
