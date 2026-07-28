export type IncidentStyle = "info" | "warning" | "danger" | "success";
export type OverallLevel = "operational" | "degraded" | "outage";

export interface IncidentSeverity {
  style: IncidentStyle;
  label: string;
  rank: number;
  card: string;
  chip: string;
  badge: string;
}

export interface HealthMonitor {
  status: string;
}

export interface HealthIncident {
  style?: string;
}

const SEVERITY: Record<IncidentStyle, IncidentSeverity> = {
  danger: {
    style: "danger",
    label: "Danger",
    rank: 3,
    card: "border-l-danger bg-danger/[0.06]",
    chip: "bg-danger/15 text-danger",
    badge: "border-danger/25 bg-danger/10 text-danger",
  },
  warning: {
    style: "warning",
    label: "Warning",
    rank: 2,
    card: "border-l-warning bg-warning/[0.06]",
    chip: "bg-warning/15 text-warning",
    badge: "border-warning/25 bg-warning/10 text-warning",
  },
  info: {
    style: "info",
    label: "Info",
    rank: 1,
    card: "border-l-info bg-info/[0.06]",
    chip: "bg-info/15 text-info",
    badge: "border-info/25 bg-info/10 text-info",
  },
  success: {
    style: "success",
    label: "Success",
    rank: 0,
    card: "border-l-success bg-success/[0.06]",
    chip: "bg-success/15 text-success",
    badge: "border-success/25 bg-success/10 text-success",
  },
};

export const BANNER_TOKENS: Record<
  OverallLevel,
  {
    label: string;
    tint: string;
    chip: string;
    pill: string;
    dot: string;
  }
> = {
  operational: {
    label: "Operational",
    tint: "border-success/20 bg-success/[0.07]",
    chip: "bg-success/15 text-success",
    pill: "border-success/25 bg-success/10 text-success",
    dot: "dot-up",
  },
  degraded: {
    label: "Degraded",
    tint: "border-warning/20 bg-warning/[0.07]",
    chip: "bg-warning/15 text-warning",
    pill: "border-warning/25 bg-warning/10 text-warning",
    dot: "dot-warn",
  },
  outage: {
    label: "Major Outage",
    tint: "border-danger/20 bg-danger/[0.07]",
    chip: "bg-danger/15 text-danger",
    pill: "border-danger/25 bg-danger/10 text-danger",
    dot: "dot-down",
  },
};

/** Returns presentation tokens for an incident, treating absent or unknown styles as info. */
export function severityOf(style: string | undefined): IncidentSeverity {
  return SEVERITY[style as IncidentStyle] ?? SEVERITY.info;
}

/** Computes the public page's three-level health from visible monitor and incident state. */
export function computeOverall(
  monitors: readonly HealthMonitor[],
  activeIncidents: readonly HealthIncident[],
): OverallLevel {
  const hasDownMonitor = monitors.some((monitor) => monitor.status === "down");
  const hasDangerIncident = activeIncidents.some(
    (incident) => severityOf(incident.style).rank >= SEVERITY.danger.rank,
  );

  if (hasDownMonitor || hasDangerIncident) return "outage";
  if (activeIncidents.length > 0) return "degraded";
  return "operational";
}

/** Builds the explanatory banner copy for a computed overall health level. */
export function bannerCopy(
  level: OverallLevel,
  downCount: number,
  activeIncidentCount: number,
): { title: string; subtitle: string } {
  if (level === "operational") {
    return {
      title: "All Systems Operational",
      subtitle: "All services are running normally",
    };
  }
  if (level === "degraded") {
    return {
      title: "Active Incidents",
      subtitle: `${activeIncidentCount} active incident${activeIncidentCount === 1 ? "" : "s"} in progress`,
    };
  }
  if (downCount > 0) {
    return {
      title: "Major Outage",
      subtitle: `${downCount} service${downCount === 1 ? "" : "s"} reporting problems`,
    };
  }
  return {
    title: "Major Incident",
    subtitle: "A major incident is currently affecting services",
  };
}

/** Returns custom footer copy or an accurate health-derived default. */
export function footerCopy(
  customText: string | undefined,
  level: OverallLevel,
): string {
  const custom = customText?.trim();
  if (custom) return custom;

  switch (level) {
    case "operational":
      return "All systems operational";
    case "degraded":
      return "Some systems are experiencing degraded performance";
    case "outage":
      return "Some systems are currently unavailable";
  }
}
