/**
 * F2.3 escalation policy API.
 *
 * A policy is an ordered ladder of steps that runs while an alert stays firing.
 * Two rules from docs/F2.3-ESCALATION-CONTRACTS.md shape this client:
 *
 *  1. `wait_minutes` is the delay after the PREVIOUS step — for step 1, after
 *     the initial DOWN notification. It is cumulative, not an offset from the
 *     start of the outage.
 *  2. The initial DOWN notification is never part of a policy. Steps are always
 *     additional, so a policy can neither suppress nor duplicate it.
 *
 * `steps` is a REPLACE-SET on update: the stored ladder becomes exactly what
 * you send. Always send the whole thing.
 */
import { api } from "./client";

export interface EscalationStep {
  /** Server-assigned from array position; ignored on write. */
  step_order: number;
  wait_minutes: number;
  notification_ids: number[];
}

export interface EscalationPolicy {
  id: number;
  name: string;
  description: string;
  enabled: boolean;
  steps: EscalationStep[];
  created_at: string;
  updated_at: string;
}

export interface EscalationStepInput {
  wait_minutes: number;
  notification_ids: number[];
}

export interface EscalationPolicyInput {
  name: string;
  description?: string;
  enabled?: boolean;
  steps: EscalationStepInput[];
}

/** `policy_id` is 0 when nothing is assigned. */
export interface EscalationAssignment {
  policy_id: number;
}

/** One monitor or folder that is directly assigned to a policy. */
export interface EscalationEntityRef {
  id: number;
  name: string;
}

/**
 * Reverse assignment list from GET /api/escalation-policies/:id/assignments.
 * Direct assignments only — inheritance is never expanded.
 */
export interface EscalationPolicyAssignments {
  monitors: EscalationEntityRef[];
  groups: EscalationEntityRef[];
}

export const escalationApi = {
  async list(): Promise<EscalationPolicy[]> {
    return api.get<EscalationPolicy[]>("/escalation-policies");
  },

  async get(id: number): Promise<EscalationPolicy> {
    return api.get<EscalationPolicy>(`/escalation-policies/${id}`);
  },

  async create(input: EscalationPolicyInput): Promise<EscalationPolicy> {
    return api.post<EscalationPolicy>("/escalation-policies", input);
  },

  async update(
    id: number,
    input: EscalationPolicyInput,
  ): Promise<EscalationPolicy> {
    return api.put<EscalationPolicy>(`/escalation-policies/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/escalation-policies/${id}`);
  },

  /**
   * Monitors and folders directly assigned to this policy (no inheritance).
   */
  async listAssignments(
    policyId: number,
  ): Promise<EscalationPolicyAssignments> {
    return api.get<EscalationPolicyAssignments>(
      `/escalation-policies/${policyId}/assignments`,
    );
  },

  /**
   * The monitor's DIRECT assignment only — never an inherited one. Showing an
   * inherited policy in the monitor form would make saving that form silently
   * convert inheritance into a direct assignment.
   */
  async getForMonitor(monitorId: number): Promise<EscalationAssignment> {
    return api.get<EscalationAssignment>(
      `/monitors/${monitorId}/escalation-policy`,
    );
  },

  /** Pass 0 to unassign. */
  async setForMonitor(
    monitorId: number,
    policyId: number,
  ): Promise<EscalationAssignment> {
    return api.put<EscalationAssignment>(
      `/monitors/${monitorId}/escalation-policy`,
      {
        policy_id: policyId,
      },
    );
  },

  async getForGroup(groupId: number): Promise<EscalationAssignment> {
    return api.get<EscalationAssignment>(
      `/monitor-groups/${groupId}/escalation-policy`,
    );
  },

  /** Pass 0 to unassign. */
  async setForGroup(
    groupId: number,
    policyId: number,
  ): Promise<EscalationAssignment> {
    return api.put<EscalationAssignment>(
      `/monitor-groups/${groupId}/escalation-policy`,
      {
        policy_id: policyId,
      },
    );
  },
};
