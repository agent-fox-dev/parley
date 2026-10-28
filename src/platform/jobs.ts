/**
 * Work that outlives the request that asked for it. A copy of a
 * repository will not finish inside one unit of platform work, so it is
 * queued, run a batch at a time, and put back with what it has done.
 */
export interface QueuedJob {
  id: string;
  kind: string;
  payload: Record<string, unknown>;
  /** How many times this job has been taken up. */
  attempts: number;
}

export interface JobQueue {
  /** Puts work on the queue, to be taken up outside this request. */
  enqueue(kind: string, payload: Record<string, unknown>): Promise<string>;
  /** Takes the next job due, holding it so nobody else takes it. */
  claim(): Promise<QueuedJob | undefined>;
  /** Puts a job back with what it has done so far, to continue later. */
  release(id: string, payload: Record<string, unknown>): Promise<void>;
  /** The job is finished and will not be taken up again. */
  complete(id: string): Promise<void>;
  /** The job failed for good, with the reason kept for whoever asks. */
  fail(id: string, reason: string): Promise<void>;
  /** A job's state, for a caller waiting on it. */
  read(id: string): Promise<{ state: string; reason: string | null } | undefined>;
}
