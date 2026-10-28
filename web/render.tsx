import { Landing } from './pages/landing.js';

/**
 * Where JSX ends and the service begins.
 *
 * The route modules hand these plain values and take back a string
 * of HTML, so nothing outside `web/` has to be compiled as JSX.
 */
export function renderLanding(providers: Array<{ name: string }>): string {
  return `<!doctype html>${String(<Landing providers={providers} />)}`;
}
