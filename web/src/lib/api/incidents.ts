import type { Incident } from "./statuspages";
import { api } from "./client";

export type { Incident };

export const incidentsApi = {
  async list(): Promise<Incident[]> {
    return api.get<Incident[]>("/incidents");
  },

  async remove(
    incident: Pick<Incident, "id" | "status_page_id">,
  ): Promise<void> {
    return api.del(
      `/status-pages/${incident.status_page_id}/incidents/${incident.id}`,
    );
  },
};
