export type Outcome = 'pass' | 'fail' | 'skip';

export interface CheckResult {
  outcome: Outcome;
  message?: string;
}

export interface Check {
  name: string;
  /**
   * What the check cannot run without, or undefined when everything it needs
   * is present. The name it returns goes into the skip message, so an
   * operator reading the report knows what to install or start.
   */
  needs?: () => string | undefined;
  run: (context: { baseUrl: string }) => Promise<CheckResult>;
}

export interface HarnessReportEntry {
  name: string;
  outcome: Outcome;
  message?: string;
}

export interface HarnessResult {
  report: HarnessReportEntry[];
  exitCode: number;
}

/**
 * Runs the registered checks against one deployment and reports each. A
 * check whose dependency is missing is skipped, and a skip leaves the run's
 * outcome exactly as it would have been had the check not been defined:
 * only a failure makes the exit code non-zero.
 */
export async function runHarness(options: {
  baseUrl: string;
  checks: Check[];
}): Promise<HarnessResult> {
  const report: HarnessReportEntry[] = [];

  // Sequential on purpose: the checks share one deployment, and running them
  // at once would measure its concurrency rather than its conformance.
  for (const check of options.checks) {
    const missing = check.needs?.();
    if (missing !== undefined) {
      report.push({
        name: check.name,
        outcome: 'skip',
        message: `skipped: ${missing} is not available`,
      });
      continue;
    }

    try {
      const result = await check.run({ baseUrl: options.baseUrl });
      report.push({
        name: check.name,
        outcome: result.outcome,
        ...(result.message === undefined ? {} : { message: result.message }),
      });
    } catch (err) {
      report.push({
        name: check.name,
        outcome: 'fail',
        message: err instanceof Error ? err.message : String(err),
      });
    }
  }

  return { report, exitCode: report.some((entry) => entry.outcome === 'fail') ? 1 : 0 };
}

/** Renders a report for a person reading the gate's output. */
export function formatReport(result: HarnessResult): string {
  const lines = result.report.map((entry) => {
    const suffix = entry.message === undefined ? '' : ` — ${entry.message}`;
    return `  ${entry.outcome.toUpperCase().padEnd(4)} ${entry.name}${suffix}`;
  });
  const counts = { pass: 0, fail: 0, skip: 0 };
  for (const entry of result.report) {
    counts[entry.outcome] += 1;
  }
  lines.push(`  ${counts.pass} passed, ${counts.fail} failed, ${counts.skip} skipped`);
  return lines.join('\n');
}
