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
  /**
   * Headers the answer must carry. Only a refusal that is itself an
   * instruction has any — a rate limit's Retry-After — and the
   * envelope is the same either way.
   */
  readonly headers: Readonly<Record<string, string>> | undefined;

  constructor(
    status: number,
    message: string,
    errorType?: string,
    headers?: Readonly<Record<string, string>>,
  ) {
    super(message);
    this.name = 'HttpError';
    this.status = status;
    this.errorType = errorType;
    this.headers = headers;
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
    headers: { 'content-type': 'application/json; charset=UTF-8', ...err.headers },
  });
}
