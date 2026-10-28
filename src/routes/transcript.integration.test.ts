import { afterEach, describe, expect, it } from 'vitest';
import { type Service, MOUNT, as, identityService, withJson } from '../testutil/app.js';
import { seedApiKey, seedUser, seedWorkspace } from '../testutil/identity.js';

/**
 * One node's conversation, rebuilt from the traces an agent
 * ingested. It is assembled from records that were never written as
 * a conversation, so what is worth proving is the order, the roles
 * and what is left out.
 */
const RUN = '20260704_143022_a1b2c3';

let service: Service;

afterEach(() => {
  service.dispose();
});

async function world() {
  service = await identityService([], { clock: 'ticking' });
  const owner = await seedUser(service.store);
  await seedWorkspace(service.store, { slug: 'mine', owner_id: owner.id, status: 'ready' });
  return { ...service, key: (await seedApiKey(service.store, owner)).presented };
}

async function trace(
  held: Awaited<ReturnType<typeof world>>,
  body: Record<string, unknown>,
): Promise<void> {
  const response = await held.app.request(
    `${MOUNT}/workspaces/mine/runs/${RUN}/traces`,
    withJson('POST', body, held.key),
  );
  expect(response.status).toBe(201);
}

describe('a node’s conversation', () => {
  it('is rebuilt in order, with each trace type as its own role', async () => {
    const held = await world();
    await trace(held, {
      event_type: 'session.init',
      node_id: 'n-1',
      timestamp: '2026-07-04T14:00:00Z',
      data: { content: 'System initialization message' },
    });
    await trace(held, {
      event_type: 'assistant.message',
      node_id: 'n-1',
      timestamp: '2026-07-04T14:00:01Z',
      data: { content: 'I will look at the tests' },
    });
    await trace(held, {
      event_type: 'tool.use',
      node_id: 'n-1',
      timestamp: '2026-07-04T14:00:02Z',
      data: { tool_name: 'bash', content: 'Tool output' },
    });
    await trace(held, {
      event_type: 'tool.error',
      node_id: 'n-1',
      timestamp: '2026-07-04T14:00:03Z',
      data: { tool_name: 'bash', content: 'it failed' },
    });
    // A type that is no message, and another node's conversation.
    await trace(held, {
      event_type: 'session.result',
      node_id: 'n-1',
      timestamp: '2026-07-04T14:00:04Z',
      data: { content: 'done' },
    });
    await trace(held, {
      event_type: 'assistant.message',
      node_id: 'n-2',
      timestamp: '2026-07-04T14:00:05Z',
      data: { content: 'another node entirely' },
    });

    const body = (await (
      await held.app.request(
        `${MOUNT}/workspaces/mine/runs/${RUN}/transcript?node_id=n-1`,
        as(held.key),
      )
    ).json()) as {
      run_id: string;
      node_id: string;
      messages: Array<{ role: string; content: string; tool_name: string | null }>;
    };

    expect(body.run_id).toBe(RUN);
    expect(body.node_id).toBe('n-1');
    expect(body.messages.map((one) => one.role)).toEqual([
      'system',
      'assistant',
      'tool_use',
      'tool_error',
    ]);
    expect(body.messages.map((one) => one.tool_name)).toEqual([null, null, 'bash', 'bash']);
    expect(body.messages[0]?.content).toBe('System initialization message');
    expect(body.messages.map((one) => one.content)).not.toContain('another node entirely');
  });
});
