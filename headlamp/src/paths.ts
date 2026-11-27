// Pure helpers, without Headlamp imports, so they can be tested alone.

/** A version of an object, as mnemo's /api/v1/history returns it. */
export type Version = {
  id: number;
  resourceVersion: string;
  time: string;
  type: 'baseline' | 'added' | 'modified' | 'deleted';
  author: string;
  source: string;
  drift: boolean;
  paths?: string[];
};

/** Where the hub is: a Headlamp cluster (null: the current one) and a service-proxy path. */
export type Hub = { cluster: string | null; base: string };

/** An object as mnemo names it. */
export type Ref = { cluster: string; group: string; kind: string; namespace: string; name: string };

export type Service = {
  metadata: { name: string; namespace: string; labels?: Record<string, string> };
  spec: { ports?: { name?: string }[] };
};

/** proxyBase is the service-proxy path to a hub Service's http port. */
export function proxyBase(namespace: string, service: string): string {
  return `/api/v1/namespaces/${encodeURIComponent(namespace)}/services/${encodeURIComponent(
    service
  )}:http/proxy`;
}

/** pickHub chooses the hub among Services labelled app.kubernetes.io/name=mnemo. */
export function pickHub(services: Service[]): Service | undefined {
  return services.find(
    s =>
      !s.metadata.labels?.['app.kubernetes.io/component'] && // not the -metrics Service
      (s.spec.ports ?? []).some(p => p.name === 'http')
  );
}

/**
 * matchCluster is the mnemo cluster recorded with this kube-system UID. Only if the UID
 * couldn't be read does it go by name: another cluster can have the same name.
 */
export function matchCluster(
  clusters: { name: string; uid: string }[],
  uid: string | undefined,
  headlampName: string | null
): string | undefined {
  if (uid) {
    return clusters.find(c => c.uid === uid)?.name;
  }
  return clusters.find(c => headlampName && c.name === headlampName)?.name;
}

/** historyPath is the hub API path for ref's versions. */
export function historyPath(ref: Ref, limit = 20): string {
  const q = new URLSearchParams({ ...ref, limit: String(limit) });
  return `/api/v1/history?${q}`;
}

/** uiLink opens ref in mnemo's UI, as webhook links do; undefined unless uiURL is http(s). */
export function uiLink(uiURL: string, ref: Ref): string | undefined {
  if (!/^https?:\/\//i.test(uiURL)) {
    return undefined;
  }
  return `${uiURL.replace(/\/$/, '')}/?${new URLSearchParams({ ...ref })}`;
}
