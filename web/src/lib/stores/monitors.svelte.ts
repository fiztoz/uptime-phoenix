/**
 * Monitor list store — derived from the WebSocket realtime store.
 *
 * Provides convenience getters for filtered/grouped monitor lists.
 * The source of truth is always `realtime.monitors` from ws.svelte.ts.
 */
import { realtime, type Monitor } from "./ws.svelte.js";

function createMonitorsStore() {
  let searchQuery = $state("");
  let filterStatus = $state<string | null>(null);

  let filteredMonitors = $derived.by(() => {
    let list = realtime.monitors;
    if (filterStatus) {
      list = list.filter((m) => m.status === filterStatus);
    }
    if (searchQuery) {
      const q = searchQuery.toLowerCase();
      list = list.filter(
        (m) =>
          m.name.toLowerCase().includes(q) ||
          m.type.toLowerCase().includes(q) ||
          (m.target && m.target.toLowerCase().includes(q)),
      );
    }
    return list;
  });

  let upMonitors = $derived(realtime.monitors.filter((m) => m.status === "up"));
  let downMonitors = $derived(
    realtime.monitors.filter((m) => m.status === "down"),
  );
  let pendingMonitors = $derived(
    realtime.monitors.filter((m) => m.status === "pending"),
  );
  let pausedMonitors = $derived(
    realtime.monitors.filter((m) => m.status === "paused"),
  );

  let uptimePercent = $derived.by(() => {
    const total = realtime.monitors.length;
    if (total === 0) return 100;
    return Math.round((upMonitors.length / total) * 100);
  });

  function getMonitorById(id: number): Monitor | undefined {
    return realtime.monitors.find((m) => m.id === id);
  }

  function selectStatus(status: string | null) {
    filterStatus = status;
  }

  function search(query: string) {
    searchQuery = query;
  }

  return {
    get searchQuery() {
      return searchQuery;
    },
    get filterStatus() {
      return filterStatus;
    },
    get filteredMonitors() {
      return filteredMonitors;
    },
    get upMonitors() {
      return upMonitors;
    },
    get downMonitors() {
      return downMonitors;
    },
    get pendingMonitors() {
      return pendingMonitors;
    },
    get pausedMonitors() {
      return pausedMonitors;
    },
    get uptimePercent() {
      return uptimePercent;
    },
    getMonitorById,
    selectStatus,
    search,
  };
}

export const monitorsStore = createMonitorsStore();
