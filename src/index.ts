import { buildApp } from './http/router.js';

export interface Env {
  PARLEY_MOUNT_POINT?: string;
  PARLEY_BUILD_TIME?: string;
  PARLEY_COMMIT?: string;
}

let app: ReturnType<typeof buildApp> | undefined;

export default {
  fetch(request: Request, env: Env, ctx: ExecutionContext): Response | Promise<Response> {
    app ??= buildApp({
      mountPoint: env.PARLEY_MOUNT_POINT,
      buildTime: env.PARLEY_BUILD_TIME ?? '',
      commit: env.PARLEY_COMMIT ?? '',
    });
    return app.fetch(request, env, ctx);
  },
};
